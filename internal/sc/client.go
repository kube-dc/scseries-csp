// Package sc is a client for the Dell Storage Center (SC Series) REST API, as served directly by a Storage
// Center (SCOS 7.x) or by a DSM Data Collector.
package sc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Config describes one Storage Center endpoint and credentials.
type Config struct {
	Host     string
	Port     string // default 3033
	User     string
	Password string
	SSN      string // Storage Center serial when several are visible (DSM)

	// TLS: Storage Centers ship self-signed certificates (CN often an old address). Prefer Fingerprint
	// (SHA-256 of the leaf certificate, hex, colons optional, comma-separated for several controllers) or
	// CAFile; InsecureSkipVerify is for labs only.
	Fingerprint        string
	CAFile             string
	InsecureSkipVerify bool

	Timeout    time.Duration // per HTTP call, default 120s
	MaxRetries int           // for idempotent calls, default 3
}

// Error is a non-success answer from the Storage Center API.
type Error struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *Error) Error() string {
	return fmt.Sprintf("SC %s %s -> %d: %s", e.Method, e.Path, e.Status, truncate(e.Body, 400))
}

// Observer receives one call per HTTP request to the array (for metrics).
type Observer func(method, objectPath string, status int, d time.Duration)

// Client is one authenticated session against one Storage Center.
type Client struct {
	cfg     Config
	base    string
	http    *http.Client
	auth    string
	log     *slog.Logger
	observe Observer

	loginMu    sync.Mutex   // serialises logins
	state      sync.RWMutex // guards apiVersion and ssn
	apiVersion string
	ssn        string

	// Mutate serialises changes on this array (create/map/unmap/delete) inside one process.
	Mutate sync.Mutex

	cacheMu sync.Mutex
	cache   map[string]string
}

var versionRange = regexp.MustCompile(`the version must be in the range \[([\d.]+),\s*([\d.]+)\]`)

// New creates a client and logs in.
func New(ctx context.Context, cfg Config, log *slog.Logger, observe Observer) (*Client, error) {
	if cfg.Port == "" {
		cfg.Port = "3033"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if log == nil {
		log = slog.Default()
	}
	tlsCfg, err := tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(nil)
	c := &Client{
		cfg:  cfg,
		base: fmt.Sprintf("https://%s/api/rest/", net.JoinHostPort(cfg.Host, cfg.Port)),
		http: &http.Client{
			Timeout: cfg.Timeout,
			Jar:     jar,
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		auth:       "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.User+":"+cfg.Password)),
		log:        log.With("array", cfg.Host),
		observe:    observe,
		apiVersion: "4.1",
		cache:      map[string]string{},
	}
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func tlsConfig(cfg Config) (*tls.Config, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.Fingerprint != "":
		// comma-separated: each controller (and the floating management address) may present its own cert
		want := map[string]bool{}
		for _, f := range strings.Split(cfg.Fingerprint, ",") {
			if f = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(f), ":", "")); f != "" {
				want[f] = true
			}
		}
		t.InsecureSkipVerify = true // replaced by the pin check below
		t.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no peer certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if got := hex.EncodeToString(sum[:]); !want[got] {
				return fmt.Errorf("storage center certificate fingerprint %s is not pinned", got)
			}
			return nil
		}
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", cfg.CAFile)
		}
		t.RootCAs = pool
	case cfg.InsecureSkipVerify:
		t.InsecureSkipVerify = true
	}
	return t, nil
}

// SSN is the Storage Center serial number used for all filters.
func (c *Client) SSN() string {
	c.state.RLock()
	defer c.state.RUnlock()
	return c.ssn
}

func (c *Client) version() string {
	c.state.RLock()
	defer c.state.RUnlock()
	return c.apiVersion
}

// Host is the endpoint this client talks to.
func (c *Client) Host() string { return c.cfg.Host }

func (c *Client) login(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	payload := map[string]any{"Application": "scseries-csp", "ApplicationVersion": "1.0"}
	status, body, err := c.raw(ctx, http.MethodPost, "ApiConnection/Login", payload)
	if err == nil && status != http.StatusOK {
		if m := versionRange.FindStringSubmatch(string(body)); m != nil {
			c.state.Lock()
			c.apiVersion = m[2]
			c.state.Unlock()
			status, body, err = c.raw(ctx, http.MethodPost, "ApiConnection/Login", payload)
		}
	}
	if err != nil {
		return fmt.Errorf("storage center login: %w", err)
	}
	if status != http.StatusOK {
		return &Error{Method: "POST", Path: "ApiConnection/Login", Status: status, Body: string(body)}
	}
	status, body, err = c.raw(ctx, http.MethodGet, "StorageCenter/StorageCenter", nil)
	if err != nil {
		return fmt.Errorf("list storage centers: %w", err)
	}
	if status != http.StatusOK {
		return &Error{Method: "GET", Path: "StorageCenter/StorageCenter", Status: status, Body: string(body)}
	}
	var scs []StorageCenter
	if err := decodeFirst(body, &scs); err != nil {
		return fmt.Errorf("decode storage centers: %w", err)
	}
	ssn := ""
	switch {
	case c.cfg.SSN != "":
		for _, s := range scs {
			if s.ScSerialNumber.String() == c.cfg.SSN {
				ssn = c.cfg.SSN
			}
		}
		if ssn == "" {
			return fmt.Errorf("storage center %s not visible to %s", c.cfg.SSN, c.cfg.User)
		}
	case len(scs) == 1:
		ssn = scs[0].ScSerialNumber.String()
	default:
		return fmt.Errorf("%d storage centers visible; set the serial number", len(scs))
	}
	c.state.Lock()
	c.ssn = ssn
	c.state.Unlock()
	c.log.Info("logged in", "user", c.cfg.User, "ssn", ssn, "apiVersion", c.version())
	return nil
}

// Logout ends the REST session (best effort).
func (c *Client) Logout(ctx context.Context) {
	_, _, _ = c.raw(ctx, http.MethodPost, "ApiConnection/Logout", map[string]any{})
}

// raw performs one HTTP request without retries or re-login.
func (c *Client) raw(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var rd io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("x-dell-api-version", c.version())
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		if c.observe != nil {
			c.observe(method, objectPath(path), 0, time.Since(start))
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if c.observe != nil {
		c.observe(method, objectPath(path), resp.StatusCode, time.Since(start))
	}
	return resp.StatusCode, body, err
}

// decode performs a request, re-logs in once on 401 and, for idempotent calls, retries transport errors
// and 5xx answers with exponential backoff. out may be nil.
func (c *Client) decode(ctx context.Context, method, path string, payload, out any, idempotent bool) error {
	attempts := 1
	if idempotent {
		attempts = c.cfg.MaxRetries
	}
	relogged := false
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<(i-1)) * time.Second):
			}
		}
		status, body, err := c.raw(ctx, method, path, payload)
		if err != nil {
			lastErr = fmt.Errorf("SC %s %s: %w", method, path, err)
			continue
		}
		if status == http.StatusUnauthorized && !relogged && path != "ApiConnection/Login" {
			relogged = true
			if err := c.login(ctx); err != nil {
				return err
			}
			i-- // the re-login does not count as a retry
			continue
		}
		if status >= 500 && idempotent {
			lastErr = &Error{Method: method, Path: path, Status: status, Body: string(body)}
			continue
		}
		if status < 200 || status > 299 {
			return &Error{Method: method, Path: path, Status: status, Body: string(body)}
		}
		if out == nil || len(bytes.TrimSpace(body)) == 0 {
			return nil
		}
		return decodeFirst(body, out)
	}
	return lastErr
}

// decodeFirst decodes body into out. Some SC actions (CreateReplay, CreateView) answer a one-element list
// where an object is expected, and vice versa; both are accepted.
func decodeFirst(body []byte, out any) error {
	err := json.Unmarshal(body, out)
	if err == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []json.RawMessage
		if json.Unmarshal(trimmed, &list) == nil {
			if len(list) == 0 {
				return nil
			}
			return json.Unmarshal(list[0], out)
		}
	}
	return err
}

// Get fetches path into out (retried).
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.decode(ctx, http.MethodGet, path, nil, out, true)
}

// Post performs a state-changing POST (not retried).
func (c *Client) Post(ctx context.Context, path string, payload, out any) error {
	return c.decode(ctx, http.MethodPost, path, payload, out, false)
}

// Put performs a PUT (not retried).
func (c *Client) Put(ctx context.Context, path string, payload, out any) error {
	return c.decode(ctx, http.MethodPut, path, payload, out, false)
}

// Delete performs a DELETE (not retried).
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.decode(ctx, http.MethodDelete, path, nil, nil, false)
}

// GetList is the SC query call (POST <obj>/GetList with an AND of Equals filters); it is read-only and
// therefore retried. scSerialNumber is always added.
func (c *Client) GetList(ctx context.Context, obj string, filters map[string]any, out any) error {
	f := []map[string]any{{"attributeName": "scSerialNumber", "attributeValue": c.SSN(), "filterType": "Equals"}}
	for k, v := range filters {
		f = append(f, map[string]any{"attributeName": k, "attributeValue": v, "filterType": "Equals"})
	}
	payload := map[string]any{"filter": map[string]any{"filterType": "AND", "filters": f}}
	return c.decode(ctx, http.MethodPost, "StorageCenter/"+obj+"/GetList", payload, out, true)
}

// NamedID resolves an object of type obj (storage profile, OS type, ...) by name; results are cached.
func (c *Client) NamedID(ctx context.Context, obj, name string) (string, error) {
	key := obj + "/" + name
	if id := c.cached(key); id != "" {
		return id, nil
	}
	var items []struct {
		Named
		InstanceName string `json:"instanceName"`
	}
	if err := c.GetList(ctx, obj, nil, &items); err != nil {
		return "", err
	}
	for _, it := range items {
		if it.Name == name || it.InstanceName == name {
			c.store(key, it.InstanceID)
			return it.InstanceID, nil
		}
	}
	return "", fmt.Errorf("%s %q not found on storage center %s", obj, name, c.SSN())
}

// FolderID finds or creates a folder (ScVolumeFolder / ScServerFolder) at the root.
func (c *Client) FolderID(ctx context.Context, obj, name, notes string) (string, error) {
	key := obj + "/" + name
	if id := c.cached(key); id != "" {
		return id, nil
	}
	var folders []Named
	if err := c.GetList(ctx, obj, nil, &folders); err != nil {
		return "", err
	}
	for _, f := range folders {
		if f.Name == name {
			c.store(key, f.InstanceID)
			return f.InstanceID, nil
		}
	}
	var f Named
	if err := c.Post(ctx, "StorageCenter/"+obj, map[string]any{"Name": name, "StorageCenter": c.SSN(), "Notes": notes}, &f); err != nil {
		return "", err
	}
	c.log.Info("created folder", "type", obj, "name", name, "id", f.InstanceID)
	c.store(key, f.InstanceID)
	return f.InstanceID, nil
}

// PortWWN returns the WWN of a controller port (cached).
func (c *Client) PortWWN(ctx context.Context, portID string) (string, error) {
	key := "port/" + portID
	if w := c.cached(key); w != "" {
		return w, nil
	}
	var p ControllerPort
	if err := c.Get(ctx, "StorageCenter/ScControllerPort/"+portID, &p); err != nil {
		return "", err
	}
	w := strings.ToLower(p.WWN)
	c.store(key, w)
	return w, nil
}

func (c *Client) cached(key string) string {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	return c.cache[key]
}

func (c *Client) store(key, val string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	c.cache[key] = val
}

// IsStatus reports whether err is an SC API error with the given HTTP status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// objectPath turns "StorageCenter/ScVolume/64012.12/MapToServer" into "ScVolume/MapToServer" for metrics.
func objectPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "StorageCenter/"), "/")
	out := parts[:0]
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, "0123456789") && strings.Contains(p, ".") {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "/")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
