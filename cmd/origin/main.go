// Command origin is a stand-in for whatever the cache fronts. It exists so the
// whole system runs with no external dependency, and so that miss cost and object
// size distribution are knobs rather than accidents of someone's test database.
package main

import (
	"context"
	"math"
	"os/signal"
	"syscall"
	"time"

	beladyv1 "github.com/JustinK33/newproj/gen/belady/v1"
	"github.com/JustinK33/newproj/internal/cache"
	"github.com/JustinK33/newproj/internal/config"
	"github.com/JustinK33/newproj/internal/grpcx"
	"github.com/JustinK33/newproj/internal/obs"
)

type server struct {
	beladyv1.UnimplementedOriginServer

	latency time.Duration
	jitter  time.Duration
	minSize int
	maxSize int
	alpha   float64
}

func (s *server) Fetch(ctx context.Context, req *beladyv1.FetchRequest) (*beladyv1.FetchResponse, error) {
	if d := s.delay(req.GetKey()); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &beladyv1.FetchResponse{Value: s.value(req.GetKey()), Found: true}, nil
}

// delay derives jitter from the key rather than a random source, so two runs of
// the same workload see the same miss costs and hit ratios stay comparable.
func (s *server) delay(key string) time.Duration {
	if s.jitter <= 0 {
		return s.latency
	}
	h := cache.Hash(key + "#latency")
	return s.latency + time.Duration(h%uint64(s.jitter))
}

// value returns a deterministic payload whose length follows a Pareto
// distribution. Real object sizes are heavy-tailed, and that is precisely what
// makes byte hit ratio diverge from object hit ratio: a policy can be right about
// most requests and still miss most of the bytes.
func (s *server) value(key string) []byte {
	h := cache.Hash(key + "#size")

	// Inverse-transform sampling: u is uniform in (0,1], and min*u^(-1/alpha) is
	// Pareto with the given shape.
	u := (float64(h>>11) + 1) / float64(uint64(1)<<53)
	size := float64(s.minSize) * math.Pow(1/u, 1/s.alpha)

	n := s.minSize
	switch {
	case size >= float64(s.maxSize):
		n = s.maxSize
	case size > float64(s.minSize):
		n = int(size)
	}

	v := make([]byte, n)
	// Fill with a key-derived byte so a corrupted or mixed-up value is visible in
	// a hex dump rather than silently plausible.
	fill := byte(h)
	for i := range v {
		v[i] = fill
	}
	return v
}

func main() {
	log := obs.Init("origin")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &server{
		latency: config.Duration("ORIGIN_LATENCY", 2*time.Millisecond),
		jitter:  config.Duration("ORIGIN_JITTER", 1*time.Millisecond),
		minSize: config.Int("ORIGIN_MIN_SIZE", 512),
		maxSize: config.Int("ORIGIN_MAX_SIZE", 64<<10),
		alpha:   config.Float("ORIGIN_SIZE_ALPHA", 1.5),
	}
	log.Info("origin configured",
		"latency", srv.latency.String(), "jitter", srv.jitter.String(),
		"min_size", srv.minSize, "max_size", srv.maxSize, "alpha", srv.alpha)

	go func() {
		if err := obs.Serve(ctx, config.String("DEBUG_ADDR", ":9090")); err != nil {
			log.Error("debug endpoint failed", "err", err)
		}
	}()

	g := grpcx.NewServer()
	beladyv1.RegisterOriginServer(g, srv)
	if err := grpcx.Serve(ctx, config.String("GRPC_ADDR", ":8081"), g); err != nil {
		log.Error("serve failed", "err", err)
	}
}
