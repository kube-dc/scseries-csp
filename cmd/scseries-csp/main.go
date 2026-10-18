// Command scseries-csp is the Dell SC Series Container Storage Provider for the HPE CSI Driver.
//
//	scseries-csp serve   run the CSP API (default)
//	scseries-csp audit          cross-check the array's CSP folders against the cluster ([--fix ...])
//	scseries-csp cdi-profiles   set RWX raw block defaults on KubeVirt CDI StorageProfiles (Helm hook)
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/kube-dc/scseries-csp/internal/csp"
	"github.com/kube-dc/scseries-csp/internal/monitor"
	"github.com/kube-dc/scseries-csp/internal/sc"
	"github.com/kube-dc/scseries-csp/internal/server"
)

var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		cmd = os.Args[1]
	}
	log := newLogger()
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "audit":
		err = audit(log, os.Args[2:])
	case "cdi-profiles":
		err = cdiProfiles(log, os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (serve | audit | cdi-profiles | version)", cmd)
	}
	if err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func newLogger() *slog.Logger {
	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(env("LOG_LEVEL", "info")))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v, err := strconv.ParseBool(env(k, strconv.FormatBool(def)))
	if err != nil {
		return def
	}
	return v
}

func serviceConfig() csp.Config {
	d := csp.Defaults()
	return csp.Config{
		VolumeFolder:    env("SC_VOLUME_FOLDER", d.VolumeFolder),
		ServerFolder:    env("SC_SERVER_FOLDER", d.ServerFolder),
		ServerOS:        env("SC_SERVER_OS", d.ServerOS),
		StorageProfile:  env("SC_STORAGE_PROFILE", d.StorageProfile),
		DataReduction:   env("SC_DATA_REDUCTION", d.DataReduction),
		SnapshotProfile: os.Getenv("SC_SNAPSHOT_PROFILE"),
	}
}

func arrayConfig(host, user, password string) sc.Config {
	return sc.Config{
		Host: host, Port: env("SC_PORT", "3033"), User: user, Password: password, SSN: os.Getenv("SC_SSN"),
		Fingerprint: os.Getenv("SC_TLS_FINGERPRINT"), CAFile: os.Getenv("SC_CA_FILE"),
		InsecureSkipVerify: envBool("SC_TLS_INSECURE", false),
	}
}

func tokenKey(log *slog.Logger) ([]byte, error) {
	raw := os.Getenv("CSP_TOKEN_KEY")
	if f := os.Getenv("CSP_TOKEN_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		raw = strings.TrimSpace(string(b))
	}
	if raw == "" {
		log.Warn("CSP_TOKEN_KEY not set: using a random key; tokens are not shared between replicas or restarts")
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(raw)
}

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := server.NewMetrics(reg)

	key, err := tokenKey(log)
	if err != nil {
		return fmt.Errorf("token key: %w", err)
	}
	ttl, err := time.ParseDuration(env("CSP_TOKEN_TTL", "24h"))
	if err != nil {
		return err
	}
	tokens, err := server.NewTokenCodec(key, ttl)
	if err != nil {
		return err
	}
	cfg := serviceConfig()
	svc := &csp.Service{Cfg: cfg, Log: log}
	connect := func(ctx context.Context, c server.Credentials) (*sc.Client, error) {
		ac := arrayConfig(c.Array, c.User, c.Password)
		if ac.Fingerprint == "" && ac.CAFile == "" && !ac.InsecureSkipVerify {
			return nil, errors.New("no TLS trust configured: set SC_TLS_FINGERPRINT, SC_CA_FILE or SC_TLS_INSECURE=true")
		}
		return sc.New(ctx, ac, log, metrics.Observe)
	}
	srv := server.New(svc, tokens, connect, log, metrics)

	api := &http.Server{Addr: env("CSP_LISTEN", ":8080"), Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 10 * time.Minute, IdleTimeout: 2 * time.Minute}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	met := &http.Server{Addr: env("CSP_METRICS_LISTEN", ":9090"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	if envBool("LEADER_ELECTION", false) {
		srv.SetLeader(false)
		if err := startLeaderElection(ctx, log, srv); err != nil {
			return err
		}
	} else {
		srv.SetLeader(true)
	}

	if envBool("SC_MONITOR", false) {
		host, user, pw := os.Getenv("SC_MONITOR_HOST"), os.Getenv("SC_MONITOR_USER"), os.Getenv("SC_MONITOR_PASSWORD")
		if host == "" || user == "" || pw == "" {
			return errors.New("SC_MONITOR=true needs SC_MONITOR_HOST, SC_MONITOR_USER and SC_MONITOR_PASSWORD")
		}
		interval, err := time.ParseDuration(env("SC_MONITOR_INTERVAL", "60s"))
		if err != nil {
			return err
		}
		health := monitor.NewMetrics(reg)
		go health.Run(ctx, log, interval, func(ctx context.Context) (*sc.Client, error) {
			return connect(ctx, server.Credentials{Array: host, User: user, Password: pw})
		}, srv.IsLeader)
	}

	errs := make(chan error, 2)
	go func() { errs <- api.ListenAndServe() }()
	go func() { errs <- met.ListenAndServe() }()
	log.Info("scseries-csp started", "version", version, "api", api.Addr, "metrics", met.Addr, "config", cfg,
		"leaderElection", envBool("LEADER_ELECTION", false))

	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv.SetLeader(false)
	_ = api.Shutdown(sctx) // lets in-flight array operations finish
	_ = met.Shutdown(sctx)
	srv.Close(sctx)
	return nil
}

func startLeaderElection(ctx context.Context, log *slog.Logger, srv *server.Server) error {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("leader election needs in-cluster config: %w", err)
	}
	kc, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	name := env("POD_NAME", "")
	if name == "" {
		name, _ = os.Hostname()
	}
	// identity carries the API address so standbys can forward to the leader: <pod>@<ip>:<port>
	_, port, _ := strings.Cut(env("CSP_LISTEN", ":8080"), ":")
	id := name
	if ip := os.Getenv("POD_IP"); ip != "" {
		id = fmt.Sprintf("%s@%s:%s", name, ip, port)
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metaFor(env("LEADER_ELECTION_NAME", "scseries-csp"), env("POD_NAMESPACE", "hpe-storage")),
		Client:     kc.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: lock, ReleaseOnCancel: true,
		LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) { log.Info("became leader", "id", id); srv.SetLeader(true) },
			OnStoppedLeading: func() { log.Warn("lost leadership", "id", id); srv.SetLeader(false); srv.SetLeaderAddr("") },
			OnNewLeader: func(l string) {
				log.Info("leader is", "id", l)
				if l == id {
					return
				}
				_, addr, ok := strings.Cut(l, "@")
				if !ok {
					addr = ""
				}
				srv.SetLeaderAddr(addr)
			},
		},
	})
	if err != nil {
		return err
	}
	go func() {
		for ctx.Err() == nil {
			le.Run(ctx) // returns when leadership is lost; stand by and try again
		}
	}()
	return nil
}
