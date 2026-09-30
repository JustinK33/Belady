// Package obs wires up the things every service needs to be observable:
// structured logs, a Prometheus endpoint, and pprof.
package obs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/JustinK33/Belady/internal/config"
)

// Registry is this process's metric registry. Using our own rather than the
// default keeps a stray library import from publishing metrics we did not ask for.
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// Start is the startup sequence every service shares: install the process logger,
// derive a context cancelled on SIGINT or SIGTERM, and run the debug endpoint until
// that context is done. version is the binary's -X main.version stamp, logged once.
// The returned func releases the signal handler, so callers defer it.
//
// The debug endpoint comes up before the service configures itself, so a node stuck
// at startup still has /metrics and pprof. /healthz is liveness, not readiness: it
// answers OK before the gRPC port binds.
func Start(service, version string) (*slog.Logger, context.Context, func()) {
	log := initLogger(service)
	log.Info("starting", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if err := serveDebug(ctx, config.String("DEBUG_ADDR", ":9090")); err != nil {
			log.Error("debug endpoint failed", "err", err)
		}
	}()
	return log, ctx, stop
}

// Fatal logs a startup failure and exits 2, the status internal/config uses for a
// configuration the process cannot run with.
func Fatal(log *slog.Logger, msg string, args ...any) {
	log.Error(msg, args...)
	os.Exit(2)
}

// initLogger installs a process-wide structured logger and returns it. LOG_LEVEL
// accepts debug, info, warn, error. LOG_FORMAT accepts json or text.
func initLogger(service string) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(os.Getenv("LOG_LEVEL")))); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "text") {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}

	l := slog.New(h).With("service", service)
	slog.SetDefault(l)
	enableContentionProfiles(l)
	return l
}

// enableContentionProfiles arms the runtime's mutex and block samplers when
// PROFILE_CONTENTION is set. /debug/pprof/mutex and /debug/pprof/block exist
// unconditionally, because pprof.Index serves every registered profile, but
// without this they return an empty profile and read as "no contention" rather
// than "not measured".
//
// Off by default, and it has to be: both samplers add work to every lock
// acquisition and every blocking operation in the process. That is also why a run
// with this on must not be the run that quotes throughput.
//
// ponytail: one switch, both samplers at every event. Separate rates are the
// upgrade if a process is ever too hot to profile at full sampling.
func enableContentionProfiles(l *slog.Logger) {
	if !config.Bool("PROFILE_CONTENTION", false) {
		return
	}
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
	l.Warn("contention profiling is on, which costs throughput on every lock and every block; " +
		"do not quote latency or QPS from this run")
}

// serveDebug runs the debug endpoint until ctx is cancelled. It exposes /metrics,
// /healthz and the pprof handlers on a port separate from the gRPC port, so the
// debug surface can be firewalled off without touching the data path.
func serveDebug(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	slog.Info("debug endpoint listening", "addr", addr)
	return ServeHTTP(ctx, &http.Server{Addr: addr, Handler: mux}) //nolint:gosec // G112: ServeHTTP sets ReadHeaderTimeout
}

// ServeHTTP runs srv until ctx is cancelled, then gives in-flight requests five
// seconds to finish before returning. Timeouts left at zero get defaults, so a
// slow client cannot hold a connection open indefinitely. WriteTimeout clears the
// 30-second CPU profile pprof takes by default.
func ServeHTTP(ctx context.Context, srv *http.Server) error {
	if srv.ReadHeaderTimeout == 0 {
		srv.ReadHeaderTimeout = 5 * time.Second
	}
	if srv.ReadTimeout == 0 {
		srv.ReadTimeout = 30 * time.Second
	}
	if srv.WriteTimeout == 0 {
		srv.WriteTimeout = 60 * time.Second
	}
	if srv.IdleTimeout == 0 {
		srv.IdleTimeout = 120 * time.Second
	}

	stopped := make(chan struct{})
	// ctx is already cancelled when Shutdown runs, so its grace period needs a
	// fresh context.
	go func() { //nolint:gosec // G118, see above
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped
	return nil
}

// LatencyBuckets spans 50us to ~13s. The cache hot path lives in the first few
// buckets; the tail matters more than the mean here, so the low end is dense.
var LatencyBuckets = prometheus.ExponentialBuckets(50e-6, 2, 19)
