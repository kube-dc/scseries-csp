// Package server is the HTTP side of the CSP: the HPE CSP REST routes, token handling, the leader gate,
// request logging and metrics.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kube-dc/scseries-csp/internal/csp"
	"github.com/kube-dc/scseries-csp/internal/sc"
)

// SessionFactory opens a Storage Center session (overridable in tests).
type SessionFactory func(ctx context.Context, c Credentials) (*sc.Client, error)

// Server serves the HPE CSP API.
type Server struct {
	Svc     *csp.Service
	Tokens  *TokenCodec
	Connect SessionFactory
	Log     *slog.Logger
	Metrics *Metrics

	leader     atomic.Bool
	leaderAddr atomic.Value // host:port of the current leader when this replica is standby ("" = unknown)

	mu       sync.Mutex
	sessions map[[32]byte]*sc.Client
}

// New wires a server; it starts as leader unless SetLeader(false) is called by leader election.
func New(svc *csp.Service, tokens *TokenCodec, connect SessionFactory, log *slog.Logger, m *Metrics) *Server {
	s := &Server{Svc: svc, Tokens: tokens, Connect: connect, Log: log, Metrics: m, sessions: map[[32]byte]*sc.Client{}}
	s.leader.Store(true)
	return s
}

// SetLeader flips the readiness/serving gate.
func (s *Server) SetLeader(v bool) {
	s.leader.Store(v)
	if s.Metrics != nil {
		if v {
			s.Metrics.Leader.Set(1)
		} else {
			s.Metrics.Leader.Set(0)
		}
	}
}

// IsLeader reports whether this replica may change the array.
func (s *Server) IsLeader() bool { return s.leader.Load() }

// SetLeaderAddr records where the current leader serves (from the Lease holder identity).
func (s *Server) SetLeaderAddr(addr string) { s.leaderAddr.Store(addr) }

func (s *Server) leaderAddress() string {
	a, _ := s.leaderAddr.Load().(string)
	return a
}

// forwardedHeader marks a request proxied from a standby; it is never forwarded again.
const forwardedHeader = "X-Scseries-Csp-Forwarded"

// Close logs out of all array sessions.
func (s *Server) Close(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, c := range s.sessions {
		c.Logout(ctx)
		delete(s.sessions, k)
	}
}

func (s *Server) session(ctx context.Context, c Credentials) (*sc.Client, error) {
	key := sha256.Sum256([]byte(c.Array + "\x00" + c.User + "\x00" + c.Password))
	s.mu.Lock()
	cl := s.sessions[key]
	s.mu.Unlock()
	if cl != nil {
		return cl, nil
	}
	cl, err := s.Connect(ctx, c)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.sessions[key]; existing != nil {
		cl.Logout(ctx)
		return existing, nil
	}
	s.sessions[key] = cl
	return cl, nil
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case s.IsLeader():
			writeJSON(w, 200, map[string]string{"status": "leader"})
		case s.leaderAddress() != "":
			writeJSON(w, 200, map[string]string{"status": "standby", "leader": s.leaderAddress()})
		default:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "no leader"})
		}
	})

	const p = "/containers/v1"
	mux.HandleFunc("POST "+p+"/tokens", s.wrap("tokens", s.postToken, false))
	mux.HandleFunc("DELETE "+p+"/tokens/{id}", s.wrap("tokens", func(*http.Request, *sc.Client) (any, int, error) { return nil, 204, nil }, false))

	mux.HandleFunc("POST "+p+"/hosts", s.wrap("hosts", s.postHost, true))
	mux.HandleFunc("GET "+p+"/hosts/{id}", s.wrap("hosts", func(r *http.Request, c *sc.Client) (any, int, error) {
		h, err := s.Svc.GetHost(r.Context(), c, r.PathValue("id"))
		return h, 200, err
	}, true))
	mux.HandleFunc("DELETE "+p+"/hosts/{id}", s.wrap("hosts", func(*http.Request, *sc.Client) (any, int, error) { return nil, 204, nil }, true))

	mux.HandleFunc("GET "+p+"/volumes", s.wrap("volumes", func(r *http.Request, c *sc.Client) (any, int, error) {
		v, err := s.Svc.ListVolumes(r.Context(), c, r.URL.Query().Get("name"))
		return v, 200, err
	}, true))
	mux.HandleFunc("POST "+p+"/volumes", s.wrap("volumes", func(r *http.Request, c *sc.Client) (any, int, error) {
		var req csp.CreateVolumeRequest
		if err := decode(r, &req); err != nil {
			return nil, 0, err
		}
		v, err := s.Svc.CreateVolume(r.Context(), c, req)
		return v, 200, err
	}, true))
	mux.HandleFunc("GET "+p+"/volumes/{id}", s.wrap("volume", func(r *http.Request, c *sc.Client) (any, int, error) {
		v, err := s.Svc.GetVolume(r.Context(), c, r.PathValue("id"))
		return v, 200, err
	}, true))
	mux.HandleFunc("PUT "+p+"/volumes/{id}", s.wrap("volume", func(r *http.Request, c *sc.Client) (any, int, error) {
		var req struct {
			Size int64 `json:"size"`
		}
		if err := decode(r, &req); err != nil {
			return nil, 0, err
		}
		v, err := s.Svc.ExpandVolume(r.Context(), c, r.PathValue("id"), req.Size)
		return v, 200, err
	}, true))
	mux.HandleFunc("DELETE "+p+"/volumes/{id}", s.wrap("volume", func(r *http.Request, c *sc.Client) (any, int, error) {
		return nil, 204, s.Svc.DeleteVolume(r.Context(), c, r.PathValue("id"))
	}, true))
	mux.HandleFunc("PUT "+p+"/volumes/{id}/actions/publish", s.wrap("publish", func(r *http.Request, c *sc.Client) (any, int, error) {
		var req csp.PublishRequest
		if err := decode(r, &req); err != nil {
			return nil, 0, err
		}
		info, err := s.Svc.Publish(r.Context(), c, r.PathValue("id"), req)
		return info, 200, err
	}, true))
	mux.HandleFunc("PUT "+p+"/volumes/{id}/actions/unpublish", s.wrap("unpublish", func(r *http.Request, c *sc.Client) (any, int, error) {
		var req csp.PublishRequest
		if err := decode(r, &req); err != nil {
			return nil, 0, err
		}
		return nil, 204, s.Svc.Unpublish(r.Context(), c, r.PathValue("id"), req)
	}, true))

	mux.HandleFunc("GET "+p+"/snapshots", s.wrap("snapshots", func(r *http.Request, c *sc.Client) (any, int, error) {
		q := r.URL.Query()
		v, err := s.Svc.ListSnapshots(r.Context(), c, q.Get("volume_id"), q.Get("name"))
		return v, 200, err
	}, true))
	mux.HandleFunc("POST "+p+"/snapshots", s.wrap("snapshots", func(r *http.Request, c *sc.Client) (any, int, error) {
		var req csp.SnapshotRequest
		if err := decode(r, &req); err != nil {
			return nil, 0, err
		}
		v, err := s.Svc.CreateSnapshot(r.Context(), c, req)
		return v, 200, err
	}, true))
	mux.HandleFunc("GET "+p+"/snapshots/{id}", s.wrap("snapshot", func(r *http.Request, c *sc.Client) (any, int, error) {
		v, err := s.Svc.GetSnapshot(r.Context(), c, r.PathValue("id"))
		return v, 200, err
	}, true))
	mux.HandleFunc("DELETE "+p+"/snapshots/{id}", s.wrap("snapshot", func(r *http.Request, c *sc.Client) (any, int, error) {
		return nil, 204, s.Svc.DeleteSnapshot(r.Context(), c, r.PathValue("id"))
	}, true))

	notSupported := s.wrap("unsupported", func(*http.Request, *sc.Client) (any, int, error) {
		return nil, 0, &csp.Error{Status: http.StatusNotImplemented, Msg: "volume groups and snapshot groups are not supported"}
	}, false)
	for _, r := range []string{"volume_groups", "snapshot_groups"} {
		mux.HandleFunc(p+"/"+r, notSupported)
		mux.HandleFunc(p+"/"+r+"/", notSupported)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "no route for "+r.Method+" "+r.URL.Path)
	})
	return mux
}

type handlerFunc func(r *http.Request, c *sc.Client) (any, int, error)

// wrap adds the leader gate, token check, error mapping, logging and metrics.
func (s *Server) wrap(route string, h handlerFunc, needSession bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		status, err := s.serve(w, r, h, needSession)
		d := time.Since(start)
		if s.Metrics != nil {
			s.Metrics.Requests.WithLabelValues(route, r.Method, strconv.Itoa(status)).Inc()
			s.Metrics.Duration.WithLabelValues(route, r.Method).Observe(d.Seconds())
		}
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", status, "ms", d.Milliseconds()}
		switch {
		case status >= 500:
			s.Log.Error("request", append(attrs, "err", err)...)
		case status >= 400:
			s.Log.Warn("request", append(attrs, "err", err)...)
		default:
			s.Log.Info("request", attrs...)
		}
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, h handlerFunc, needSession bool) (int, error) {
	if !s.IsLeader() {
		addr := s.leaderAddress()
		if addr == "" || r.Header.Get(forwardedHeader) != "" {
			writeErr(w, http.StatusServiceUnavailable, "no leader available")
			return http.StatusServiceUnavailable, errors.New("standby without leader")
		}
		return s.forward(w, r, addr)
	}
	var c *sc.Client
	if needSession {
		cred, err := s.Tokens.Open(r.Header.Get("x-auth-token"))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return http.StatusUnauthorized, err
		}
		if a := r.Header.Get("x-array-ip"); a != "" && a != cred.Array {
			writeErr(w, http.StatusUnauthorized, "x-array-ip does not match token")
			return http.StatusUnauthorized, errors.New("array mismatch")
		}
		if c, err = s.session(r.Context(), cred); err != nil {
			return s.fail(w, err)
		}
	}
	out, status, err := h(r, c)
	if err != nil {
		return s.fail(w, err)
	}
	if out == nil || status == http.StatusNoContent {
		w.WriteHeader(status)
		return status, nil
	}
	writeJSON(w, status, out)
	return status, nil
}

// forward proxies a request from a standby replica to the leader.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, addr string) (int, error) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	var perr error
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: addr})
			pr.Out.Header.Set(forwardedHeader, "1")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			perr = err
			writeErr(w, http.StatusServiceUnavailable, "leader unreachable: "+err.Error())
		},
	}
	proxy.ServeHTTP(rec, r)
	return rec.status, perr
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) fail(w http.ResponseWriter, err error) (int, error) {
	var ce *csp.Error
	var se *sc.Error
	status := http.StatusInternalServerError
	switch {
	case errors.As(err, &ce):
		status = ce.Status
	case errors.As(err, &se) && se.Status == http.StatusUnauthorized:
		status = http.StatusUnauthorized
	case errors.As(err, &se) && se.Status >= 500:
		status = http.StatusServiceUnavailable // array busy/failing: let the driver retry
	case errors.As(err, &se):
		status = http.StatusBadGateway
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusServiceUnavailable
	}
	writeErr(w, status, err.Error())
	return status, err
}

func (s *Server) postToken(r *http.Request, _ *sc.Client) (any, int, error) {
	var req struct {
		ArrayIP  string `json:"array_ip"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		return nil, 0, err
	}
	if req.ArrayIP == "" || req.Username == "" || req.Password == "" {
		return nil, 0, &csp.Error{Status: http.StatusBadRequest, Msg: "array_ip, username and password are required"}
	}
	cred := Credentials{Array: req.ArrayIP, User: req.Username, Password: req.Password}
	if _, err := s.session(r.Context(), cred); err != nil { // validates the credentials
		var se *sc.Error
		if errors.As(err, &se) && se.Status == http.StatusUnauthorized {
			return nil, 0, &csp.Error{Status: http.StatusUnauthorized, Msg: "storage center rejected the credentials"}
		}
		return nil, 0, err
	}
	tok, exp, err := s.Tokens.Seal(cred)
	if err != nil {
		return nil, 0, err
	}
	now := time.Now().Unix()
	return map[string]any{"id": fmt.Sprintf("%x", sha256.Sum256([]byte(tok)))[:16], "session_token": tok,
		"array_ip": req.ArrayIP, "username": req.Username, "creation_time": now, "expiry_time": exp}, 200, nil
}

func (s *Server) postHost(r *http.Request, c *sc.Client) (any, int, error) {
	var req csp.HostRequest
	if err := decode(r, &req); err != nil {
		return nil, 0, err
	}
	h, err := s.Svc.AssertHost(r.Context(), c, req)
	return h, 200, err
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil && err.Error() != "EOF" {
		return &csp.Error{Status: http.StatusBadRequest, Msg: "invalid JSON body: " + err.Error()}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"errors": []map[string]string{{"code": http.StatusText(status), "message": msg}}})
}

// Metrics are the Prometheus collectors of the CSP.
type Metrics struct {
	Requests   *prometheus.CounterVec
	Duration   *prometheus.HistogramVec
	SCRequests *prometheus.CounterVec
	SCDuration *prometheus.HistogramVec
	Leader     prometheus.Gauge
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scseries_csp_requests_total", Help: "CSP API requests by route, method and HTTP status."},
			[]string{"route", "method", "code"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "scseries_csp_request_duration_seconds", Help: "CSP API request latency.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}}, []string{"route", "method"}),
		SCRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scseries_csp_array_requests_total", Help: "Storage Center REST calls by object path and HTTP status (0 = transport error)."},
			[]string{"method", "object", "code"}),
		SCDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "scseries_csp_array_request_duration_seconds", Help: "Storage Center REST call latency.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30}}, []string{"method", "object"}),
		Leader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "scseries_csp_leader", Help: "1 if this replica is the active leader."}),
	}
	reg.MustRegister(m.Requests, m.Duration, m.SCRequests, m.SCDuration, m.Leader)
	return m
}

// Observe is an sc.Observer feeding the array metrics.
func (m *Metrics) Observe(method, object string, status int, d time.Duration) {
	m.SCRequests.WithLabelValues(method, object, strconv.Itoa(status)).Inc()
	m.SCDuration.WithLabelValues(method, object).Observe(d.Seconds())
}
