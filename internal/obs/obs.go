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
	"runtime"
	"strings"
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

// Init installs a process-wide structured logger and returns it. LOG_LEVEL
// accepts debug, info, warn, error. LOG_FORMAT accepts json or text.
func Init(service string) *slog.Logger {
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
// One switch, and both samplers set to report every event. A diagnostic run wants
// the whole picture; tuning the sampling rates is a problem for a process too hot
// to profile at all, which is not this one.
func enableContentionProfiles(l *slog.Logger) {
	if !config.Bool("PROFILE_CONTENTION", false) {
		return
	}
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
	l.Warn("contention profiling is on, which costs throughput on every lock and every block; " +
		"do not quote latency or QPS from this run")
}

// Serve runs the debug endpoint until ctx is cancelled. It exposes /metrics,
// /healthz and the pprof handlers on a port separate from the gRPC port, so the
// debug surface can be firewalled off without touching the data path.
func Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The shutdown context below is deliberately not derived from ctx: ctx is already
	// cancelled by the time this goroutine wakes, and a cancelled context gives
	// Shutdown no grace period at all.
	go func() { //nolint:gosec // G118, see above
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	slog.Info("debug endpoint listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// LatencyBuckets spans 50us to ~13s. The cache hot path lives in the first few
// buckets; the tail matters more than the mean here, so the low end is dense.
var LatencyBuckets = prometheus.ExponentialBuckets(50e-6, 2, 19)
