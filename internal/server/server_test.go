package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kube-dc/scseries-csp/internal/csp"
	"github.com/kube-dc/scseries-csp/internal/sc"
	"github.com/kube-dc/scseries-csp/internal/sc/scfake"
)

func newTestServer(t *testing.T, key []byte) (*Server, *httptest.Server, *scfake.Array) {
	t.Helper()
	a := scfake.New()
	t.Cleanup(a.Close)
	host, port := a.HostPort()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens, err := NewTokenCodec(key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	connect := func(ctx context.Context, c Credentials) (*sc.Client, error) {
		return sc.New(ctx, sc.Config{Host: host, Port: port, User: c.User, Password: c.Password, InsecureSkipVerify: true}, log, nil)
	}
	s := New(&csp.Service{Cfg: csp.Defaults(), Log: log}, tokens, connect, log, NewMetrics(prometheus.NewRegistry()))
	h := httptest.NewServer(s.Handler())
	t.Cleanup(h.Close)
	return s, h, a
}

func call(t *testing.T, h *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.URL+path, rd)
	if token != "" {
		req.Header.Set("x-auth-token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func login(t *testing.T, h *httptest.Server, a *scfake.Array) string {
	t.Helper()
	st, out := call(t, h, "POST", "/containers/v1/tokens", "", map[string]string{"array_ip": "x", "username": a.User, "password": a.Password})
	if st != 200 {
		t.Fatalf("token: %d %v", st, out)
	}
	return out["session_token"].(string)
}

func TestTokenRoundTripAcrossReplicas(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	_, h1, a := newTestServer(t, key)
	tok := login(t, h1, a)
	// a second replica with the same key accepts the token without its own login call
	_, h2, _ := newTestServer(t, key)
	if st, _ := call(t, h2, "GET", "/containers/v1/volumes", tok, nil); st == http.StatusUnauthorized {
		t.Fatal("token from replica 1 rejected by replica 2")
	}
	// a replica with a different key rejects it -> driver re-authenticates
	_, h3, _ := newTestServer(t, bytes.Repeat([]byte{9}, 32))
	if st, _ := call(t, h3, "GET", "/containers/v1/volumes", tok, nil); st != http.StatusUnauthorized {
		t.Fatalf("foreign token accepted: %d", st)
	}
}

func TestBadCredentialsAndTamperedToken(t *testing.T) {
	_, h, a := newTestServer(t, nil)
	if st, _ := call(t, h, "POST", "/containers/v1/tokens", "", map[string]string{"array_ip": "x", "username": a.User, "password": "wrong"}); st != 401 {
		t.Errorf("wrong password should be 401, got %d", st)
	}
	tok := login(t, h, a)
	if st, _ := call(t, h, "GET", "/containers/v1/volumes", tok[:len(tok)-2]+"xx", nil); st != 401 {
		t.Errorf("tampered token should be 401, got %d", st)
	}
	codec, _ := NewTokenCodec(nil, -time.Second)
	expired, _, _ := codec.Seal(Credentials{Array: "x", User: "u", Password: "p"})
	if _, err := codec.Open(expired); err == nil {
		t.Error("expired token accepted")
	}
}

func TestStandbyRefusesWork(t *testing.T) {
	s, h, a := newTestServer(t, nil)
	tok := login(t, h, a)
	s.SetLeader(false)
	if st, _ := call(t, h, "GET", "/readyz", "", nil); st != 503 {
		t.Errorf("standby readyz should be 503, got %d", st)
	}
	if st, out := call(t, h, "POST", "/containers/v1/volumes", tok, map[string]any{"name": "pvc-1", "size": 1 << 30}); st != 503 || out["errors"] == nil {
		t.Errorf("standby must refuse API calls with a CSP error body, got %d %v", st, out)
	}
	s.SetLeader(true)
	if st, _ := call(t, h, "GET", "/readyz", "", nil); st != 200 {
		t.Errorf("leader readyz should be 200, got %d", st)
	}
}

func TestVolumeLifecycleOverHTTP(t *testing.T) {
	_, h, a := newTestServer(t, nil)
	tok := login(t, h, a)
	st, vol := call(t, h, "POST", "/containers/v1/volumes", tok, map[string]any{"name": "pvc-http", "size": 1 << 30, "config": map[string]any{}})
	if st != 200 || vol["id"] == nil {
		t.Fatalf("create: %d %v", st, vol)
	}
	id := vol["id"].(string)
	if st, _ := call(t, h, "GET", "/containers/v1/volumes?name=nope", tok, nil); st != 404 {
		t.Errorf("missing name should be 404, got %d", st)
	}
	if st, _ := call(t, h, "POST", "/containers/v1/hosts", tok, map[string]any{"name": "n1", "uuid": "u1", "wwpns": []string{"aa01"}}); st != 200 {
		t.Fatalf("host: %d", st)
	}
	st, info := call(t, h, "PUT", "/containers/v1/volumes/"+id+"/actions/publish", tok, map[string]any{"host_uuid": "u1", "access_protocol": "fc"})
	if st != 200 || info["serial_number"] == "" || info["lun_id"] == nil {
		t.Fatalf("publish: %d %v", st, info)
	}
	if st, _ := call(t, h, "PUT", "/containers/v1/volumes/"+id+"/actions/unpublish", tok, map[string]any{"host_uuid": "u1"}); st != 204 {
		t.Errorf("unpublish: %d", st)
	}
	if st, _ := call(t, h, "DELETE", "/containers/v1/volumes/"+id, tok, nil); st != 204 {
		t.Errorf("delete: %d", st)
	}
	if st, out := call(t, h, "GET", "/containers/v1/volume_groups", tok, nil); st != 501 || out["errors"] == nil {
		t.Errorf("volume groups should be 501 with error body, got %d", st)
	}
}

func TestStandbyForwardsToLeader(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	_, leaderHTTP, a := newTestServer(t, key)
	standby, standbyHTTP, _ := newTestServer(t, key)
	tok := login(t, leaderHTTP, a)
	standby.SetLeader(false)
	if st, _ := call(t, standbyHTTP, "GET", "/readyz", "", nil); st != 503 {
		t.Errorf("standby without known leader must not be ready, got %d", st)
	}
	standby.SetLeaderAddr(strings.TrimPrefix(leaderHTTP.URL, "http://"))
	if st, _ := call(t, standbyHTTP, "GET", "/readyz", "", nil); st != 200 {
		t.Errorf("standby with known leader should be ready, got %d", st)
	}
	st, vol := call(t, standbyHTTP, "POST", "/containers/v1/volumes", tok, map[string]any{"name": "pvc-fwd", "size": 1 << 30})
	if st != 200 || vol["id"] == nil {
		t.Fatalf("request via standby should be served by the leader: %d %v", st, vol)
	}
	// a request already forwarded is never forwarded again (no loops)
	req, _ := http.NewRequest("GET", standbyHTTP.URL+"/containers/v1/volumes", nil)
	req.Header.Set("x-auth-token", tok)
	req.Header.Set("X-Scseries-Csp-Forwarded", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 503 {
		t.Errorf("forwarded request at a standby must be 503, got %v %v", resp.StatusCode, err)
	}
}

// TestConcurrentRequests drives the API the way parallel CSI sidecars do and is meant to run under -race.
func TestConcurrentRequests(t *testing.T) {
	srv, h, a := newTestServer(t, bytes.Repeat([]byte{5}, 32))

	// concurrent first logins race on the session cache
	tokens := make([]string, 8)
	var wg sync.WaitGroup
	for i := range tokens {
		wg.Add(1)
		go func(i int) { defer wg.Done(); tokens[i] = login(t, h, a) }(i)
	}
	wg.Wait()

	// leader address updates while requests are in flight (stays leader, so no effect on results)
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				srv.SetLeaderAddr(fmt.Sprintf("10.0.0.%d:8080", i%250))
				_ = srv.IsLeader()
			}
		}
	}()
	defer close(stop)

	// 20 simultaneous creates of the same PVC name must yield exactly one volume
	ids := make([]string, 20)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, v := call(t, h, "POST", "/containers/v1/volumes", tokens[i%len(tokens)], map[string]any{"name": "pvc-same", "size": 1 << 30})
			if st == 200 {
				ids[i], _ = v["id"].(string)
			}
		}(i)
	}
	wg.Wait()
	for i, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("create %d returned %q, want the single volume %q", i, id, ids[0])
		}
	}
	if st, _ := call(t, h, "GET", "/containers/v1/volumes?name=pvc-same", tokens[0], nil); st != 200 {
		t.Fatalf("lookup: %d", st)
	}
	n := 0
	for _, v := range a.VolumeNames() {
		if v == "pvc-same" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d volumes named pvc-same on the array, want exactly 1", n)
	}

	// two hosts, 10 volumes published to both concurrently
	for _, n := range []struct{ name, uuid, wwpn string }{{"na", "ua", "aa01"}, {"nb", "ub", "bb01"}} {
		if st, _ := call(t, h, "POST", "/containers/v1/hosts", tokens[0], map[string]any{"name": n.name, "uuid": n.uuid, "wwpns": []string{n.wwpn}}); st != 200 {
			t.Fatalf("host %s: %d", n.name, st)
		}
	}
	vols := make([]string, 10)
	for i := range vols {
		_, v := call(t, h, "POST", "/containers/v1/volumes", tokens[0], map[string]any{"name": fmt.Sprintf("pvc-par-%d", i), "size": 1 << 30})
		vols[i] = v["id"].(string)
	}
	var mu sync.Mutex
	failures := 0
	for _, id := range vols {
		for _, host := range []string{"ua", "ub"} {
			wg.Add(1)
			go func(id, host string) {
				defer wg.Done()
				st, _ := call(t, h, "PUT", "/containers/v1/volumes/"+id+"/actions/publish", tokens[1], map[string]any{"host_uuid": host, "access_protocol": "fc"})
				if st != 200 {
					mu.Lock()
					failures++
					mu.Unlock()
				}
			}(id, host)
		}
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("%d concurrent publishes failed", failures)
	}
	for _, id := range vols {
		if got := a.Mappings(id); len(got) != 2 {
			t.Errorf("volume %s mapped to %v, want both hosts exactly once", id, got)
		}
	}
}
