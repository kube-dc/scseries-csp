package sc_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/kube-dc/scseries-csp/internal/sc"
	"github.com/kube-dc/scseries-csp/internal/sc/scfake"
)

func TestTLSFingerprintPinning(t *testing.T) {
	a := scfake.New()
	defer a.Close()
	host, port := a.HostPort()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := sc.Config{Host: host, Port: port, User: a.User, Password: a.Password}
	ctx := context.Background()

	cfg.Fingerprint = "00:11," + strings.ToUpper(a.Fingerprint()) // list, colons and case tolerated
	if _, err := sc.New(ctx, cfg, log, nil); err != nil {
		t.Fatalf("pinned fingerprint rejected: %v", err)
	}
	cfg.Fingerprint = strings.Repeat("ab", 32)
	if _, err := sc.New(ctx, cfg, log, nil); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("wrong fingerprint must fail, got %v", err)
	}
	cfg.Fingerprint = ""
	if _, err := sc.New(ctx, cfg, log, nil); err == nil {
		t.Fatal("self-signed certificate accepted without pin, CA or insecure flag")
	}
}

func TestReloginOnExpiredSession(t *testing.T) {
	a := scfake.New()
	defer a.Close()
	host, port := a.HostPort()
	c, err := sc.New(context.Background(), sc.Config{Host: host, Port: port, User: a.User, Password: a.Password, InsecureSkipVerify: true},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	// rotate the array password: the session's next call gets 401, re-login fails cleanly with a 401 error
	a.Password = "rotated"
	var out []sc.Volume
	if err := c.GetList(context.Background(), "ScVolume", nil, &out); !sc.IsStatus(err, 401) {
		t.Fatalf("expected a 401 after credential rotation, got %v", err)
	}
}
