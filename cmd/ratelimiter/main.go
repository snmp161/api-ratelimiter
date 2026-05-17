package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"

	"ratelimiter/internal/admin"
	"ratelimiter/internal/cleanup"
	"ratelimiter/internal/config"
	"ratelimiter/internal/counter"
	"ratelimiter/internal/handler"
	"ratelimiter/internal/limiter"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/store"
)

// Version is replaced at build time via -ldflags. Default "dev" when built
// outside a git tree.
var Version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Default()
	cfg.BindFlags(pflag.CommandLine)
	pflag.Parse()

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	logger.Info("ratelimiter starting",
		"version", Version,
		"listen", cfg.Listen,
		"socket_mode", cfg.SocketMode,
		"admin", cfg.AdminListen,
		"metrics", cfg.MetricsListen,
		"redis", cfg.RedisAddr,
		"global_limit", cfg.GlobalLimit,
		"burst", cfg.Burst,
		"window", cfg.Window,
		"cleanup_interval_min", cfg.CleanupInterval,
		"abuse_ttl_min", cfg.AbuseTTL,
		"abuse_multiplier", cfg.AbuseMultiplier,
		"abuse_transfer_threshold", cfg.AbuseTransferThreshold,
	)

	rdb := store.New(cfg.RedisAddr, cfg.RedisPassword, logger)
	defer rdb.Close()

	known := counter.NewKnownMap(cfg.WindowSeconds(), nil)
	unknown := counter.NewUnknownMap(cfg.WindowSeconds(), nil)
	m := metrics.New()

	lim := limiter.New(known, unknown, rdb, m, logger,
		int64(cfg.GlobalLimit), int64(cfg.Burst), int64(cfg.AbuseMultiplier))

	checkH := handler.NewCheck(lim, m, logger)

	cl := cleanup.New(known, unknown, rdb, m, logger,
		int64(cfg.AbuseTransferThreshold), cfg.AbuseTTLDuration())

	startedAt := time.Now()
	adminSrv, err := admin.New(cfg, known, unknown, rdb, m, logger, startedAt, Version)
	if err != nil {
		return fmt.Errorf("admin init: %w", err)
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	// /check listener (unix or TCP).
	checkSrv := &http.Server{
		Handler:           routes(checkH),
		ReadHeaderTimeout: 2 * time.Second,
	}
	socketMode, _ := cfg.SocketModeParsed() // already validated
	checkLn, err := listen(cfg.Listen, socketMode)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("check server listening", "addr", cfg.Listen)
		if err := checkSrv.Serve(checkLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("check server failed", "err", err)
		}
	}()

	// admin server. /healthz and /readyz are exposed alongside the admin
	// pages so orchestrators / monitors can probe on the same port.
	//
	// /healthz — liveness — always 200 while the process is running. The
	//   service is fail-open: even with Redis down it answers /check, so
	//   "alive" stays true as long as the goroutine can respond.
	// /readyz — readiness — 503 during shutdown OR when Redis is
	//   unreachable. Even though /check still works without Redis (fall
	//   back to global limit), "ready" means "full functionality" — an
	//   orchestrator can drain this instance and route to a healthy one.
	var shuttingDown atomic.Bool
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	adminMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if shuttingDown.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		pingCtx, pingCancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer pingCancel()
		if err := rdb.Ping(pingCtx); err != nil {
			http.Error(w, "redis unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	adminMux.Handle("/", adminSrv.Routes())

	adminHTTP := &http.Server{
		Addr:              cfg.AdminListen,
		Handler:           adminMux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("admin server listening", "addr", cfg.AdminListen)
		if err := adminHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin server failed", "err", err)
		}
	}()

	// metrics server.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsListen,
		Handler:           metricsMux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("metrics server listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "err", err)
		}
	}()

	// cleanup loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		cl.Loop(rootCtx, cfg.CleanupDuration())
	}()

	// Initial DB key gauges + a background ticker so they don't go stale
	// between cleanup runs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		updateRedisGauges(rootCtx, rdb, m, logger)
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-t.C:
				updateRedisGauges(rootCtx, rdb, m, logger)
			}
		}
	}()

	// shutdown handling.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	logger.Info("shutdown signal received")
	shuttingDown.Store(true) // /readyz starts answering 503 immediately

	// 1. stop accepting new /check requests.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := checkSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("check server shutdown", "err", err)
	}
	// stop admin and metrics in parallel — they're not on the hot path.
	// Log shutdown errors so a stuck-on-shutdown server is visible in
	// journald instead of getting silently SIGKILL'd by systemd after
	// TimeoutStopSec.
	go func() {
		if err := adminHTTP.Shutdown(shutdownCtx); err != nil {
			logger.Warn("admin server shutdown", "err", err)
		}
	}()
	go func() {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics server shutdown", "err", err)
		}
	}()

	// 2. final cleanup — flush abusive counters into Redis.
	logger.Info("running final cleanup")
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 10*time.Second)
	cl.Run(finalCtx)
	finalCancel()

	// 3. cancel rootCtx to stop background loops.
	cancel()
	wg.Wait()

	logger.Info("ratelimiter stopped")
	return nil
}

func routes(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/check", h)
	return mux
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// listen accepts spec strings like "unix:/path/sock" or "host:port" and
// returns a listener. socketMode is applied via chmod for unix sockets;
// it is ignored for TCP.
func listen(spec string, socketMode os.FileMode) (net.Listener, error) {
	if strings.HasPrefix(spec, "unix:") {
		path := strings.TrimPrefix(spec, "unix:")
		// Remove a stale socket left by a previous run. Safe because the
		// previous process is already gone (otherwise bind below would fail
		// after we'd just rm'd, and we'd refuse to start).
		if _, err := os.Stat(path); err == nil {
			if err := os.Remove(path); err != nil {
				return nil, fmt.Errorf("remove stale socket: %w", err)
			}
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, err
		}
		// Apply --socket-mode. Bind defaults are subject to umask, so an
		// explicit chmod is required to let other users (typically the
		// nginx/Angie process) connect.
		if err := os.Chmod(path, socketMode); err != nil {
			ln.Close()
			return nil, err
		}
		return ln, nil
	}
	return net.Listen("tcp", spec)
}

func updateRedisGauges(ctx context.Context, s *store.Store, m *metrics.Metrics, logger *slog.Logger) {
	c, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	db1, db2, db3, err := s.DBSize(c)
	if err != nil {
		m.RedisErrorsTotal.Inc()
		logger.Warn("redis dbsize failed", "err", err)
		return
	}
	m.RedisDB1Keys.Set(float64(db1))
	m.RedisDB2Keys.Set(float64(db2))
	m.RedisDB3Keys.Set(float64(db3))
}
