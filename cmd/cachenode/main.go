// Command cachenode is the data plane: one shard-set of the cache, one eviction
// policy, one connection to origin.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beladyv1 "github.com/JustinK33/newproj/gen/belady/v1"
	"github.com/JustinK33/newproj/internal/cache"
	"github.com/JustinK33/newproj/internal/config"
	"github.com/JustinK33/newproj/internal/grpcx"
	"github.com/JustinK33/newproj/internal/obs"
)

type server struct {
	beladyv1.UnimplementedCacheServer

	cache  *cache.Cache
	origin beladyv1.OriginClient

	// fetches collapses concurrent misses for the same key into one origin call.
	// Without it, a key expiring under load produces one origin request per
	// in-flight client, which is the thundering herd that takes the origin down at
	// the worst possible moment.
	fetches singleflight.Group

	nodeID string
}

func (s *server) Get(ctx context.Context, req *beladyv1.GetRequest) (*beladyv1.GetResponse, error) {
	key := req.GetKey()
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key must not be empty")
	}

	if v, ok := s.cache.Get(key); ok {
		return &beladyv1.GetResponse{Value: v, Found: true, Source: beladyv1.Source_SOURCE_CACHE, ServedBy: s.nodeID}, nil
	}

	// Do returns the shared result; only one goroutine per key reaches origin.
	v, err, _ := s.fetches.Do(key, func() (any, error) {
		resp, err := s.origin.Fetch(ctx, &beladyv1.FetchRequest{Key: key})
		if err != nil {
			return nil, err
		}
		if !resp.GetFound() {
			return nil, nil
		}
		value := resp.GetValue()
		s.cache.Admit(key, value)
		return value, nil
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "origin fetch failed: %v", err)
	}
	value, _ := v.([]byte)
	if value == nil {
		return &beladyv1.GetResponse{Found: false, ServedBy: s.nodeID}, nil
	}
	return &beladyv1.GetResponse{Value: value, Found: true, Source: beladyv1.Source_SOURCE_ORIGIN, ServedBy: s.nodeID}, nil
}

func (s *server) Put(_ context.Context, req *beladyv1.PutRequest) (*beladyv1.PutResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key must not be empty")
	}
	return &beladyv1.PutResponse{
		Admitted: s.cache.Put(req.GetKey(), req.GetValue()),
		ServedBy: s.nodeID,
	}, nil
}

func (s *server) Delete(_ context.Context, req *beladyv1.DeleteRequest) (*beladyv1.DeleteResponse, error) {
	return &beladyv1.DeleteResponse{Existed: s.cache.Delete(req.GetKey())}, nil
}

func (s *server) Stats(context.Context, *beladyv1.StatsRequest) (*beladyv1.StatsResponse, error) {
	st := s.cache.Snapshot()
	return &beladyv1.StatsResponse{
		NodeId:        s.nodeID,
		Policy:        st.Policy,
		Hits:          st.Hits,
		Misses:        st.Misses,
		HitBytes:      st.HitBytes,
		MissBytes:     st.MissBytes,
		Admissions:    st.Admissions,
		Rejections:    st.Rejections,
		Evictions:     st.Evictions,
		Objects:       st.Objects,
		BytesUsed:     st.BytesUsed,
		BytesCapacity: st.BytesCapacity,
		EvictNsMean:   st.EvictNSMean,
	}, nil
}

// newPolicy maps the configured name to a constructor. An unknown name is fatal
// rather than defaulted: silently running LRU while a dashboard claims the
// learned policy is worse than crashing.
func newPolicy(name string) (func(int64) cache.Policy, error) {
	sample := config.Int("CACHE_SAMPLE_SIZE", 8)
	switch name {
	case "lru":
		return func(int64) cache.Policy { return cache.NewLRU(sample) }, nil
	case "lfu":
		return func(int64) cache.Policy { return cache.NewLFU(sample) }, nil
	case "s3fifo":
		return cache.NewS3FIFO, nil
	default:
		return nil, fmt.Errorf("unknown CACHE_POLICY %q: want lru, lfu or s3fifo", name)
	}
}

func main() {
	log := obs.Init("cachenode")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	policyName := config.String("CACHE_POLICY", "lru")
	mkPolicy, err := newPolicy(policyName)
	if err != nil {
		log.Error("bad configuration", "err", err)
		os.Exit(2)
	}

	capacity := config.Bytes("CACHE_CAPACITY", 256<<20)
	// GOMEMLIMIT is the real ceiling; the cache should sit comfortably inside it,
	// because entry metadata, gRPC buffers and the Go heap all live in the same
	// container. Warn rather than refuse: an operator overriding this on purpose is
	// a legitimate case.
	if limit := debug.SetMemoryLimit(-1); limit > 0 && limit < capacity*2 {
		log.Warn("cache capacity is close to the process memory limit",
			"capacity", capacity, "gomemlimit", limit)
	}

	c, err := cache.New(cache.Config{
		NewPolicy:     mkPolicy,
		CapacityBytes: capacity,
		Shards:        config.Int("CACHE_SHARDS", 256),
		SampleSize:    config.Int("CACHE_SAMPLE_SIZE", 8),
	})
	if err != nil {
		log.Error("cache init failed", "err", err)
		os.Exit(2)
	}

	conn, err := grpcx.Dial(config.MustString("ORIGIN_ADDR"))
	if err != nil {
		log.Error("origin dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = conn.Close() }()

	nodeID := config.String("NODE_ID", hostnameOr("cachenode"))
	srv := &server{cache: c, origin: beladyv1.NewOriginClient(conn), nodeID: nodeID}

	registerCacheMetrics(c)
	log.Info("cache node configured", "node_id", nodeID, "policy", policyName, "capacity", capacity)

	go func() {
		if err := obs.Serve(ctx, config.String("DEBUG_ADDR", ":9090")); err != nil {
			log.Error("debug endpoint failed", "err", err)
		}
	}()

	g := grpcx.NewServer()
	beladyv1.RegisterCacheServer(g, srv)
	if err := grpcx.Serve(ctx, config.String("GRPC_ADDR", ":8081"), g); err != nil {
		log.Error("serve failed", "err", err)
	}
}

// registerCacheMetrics publishes the store's counters as gauges read on scrape.
// Collecting on demand keeps the request path free of metric updates; the store
// already counts these under a lock it was holding anyway.
func registerCacheMetrics(c *cache.Cache) {
	desc := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc("belady_cache_"+name, help, nil, nil)
	}
	specs := []struct {
		d   *prometheus.Desc
		val func(cache.Stats) float64
	}{
		{desc("hits_total", "Requests served from cache."), func(s cache.Stats) float64 { return float64(s.Hits) }},
		{desc("misses_total", "Requests not in cache."), func(s cache.Stats) float64 { return float64(s.Misses) }},
		{desc("hit_bytes_total", "Bytes served from cache."), func(s cache.Stats) float64 { return float64(s.HitBytes) }},
		{desc("miss_bytes_total", "Bytes fetched from origin."), func(s cache.Stats) float64 { return float64(s.MissBytes) }},
		{desc("evictions_total", "Objects evicted."), func(s cache.Stats) float64 { return float64(s.Evictions) }},
		{desc("rejections_total", "Objects refused admission."), func(s cache.Stats) float64 { return float64(s.Rejections) }},
		{desc("objects", "Objects currently cached."), func(s cache.Stats) float64 { return float64(s.Objects) }},
		{desc("bytes_used", "Bytes currently cached."), func(s cache.Stats) float64 { return float64(s.BytesUsed) }},
		{desc("bytes_capacity", "Configured capacity."), func(s cache.Stats) float64 { return float64(s.BytesCapacity) }},
		{desc("evict_ns_mean", "Mean nanoseconds to choose a victim, including one clock read."), func(s cache.Stats) float64 { return float64(s.EvictNSMean) }},
	}

	obs.Registry.MustRegister(collectorFunc(func(ch chan<- prometheus.Metric) {
		st := c.Snapshot()
		for _, s := range specs {
			ch <- prometheus.MustNewConstMetric(s.d, prometheus.GaugeValue, s.val(st))
		}
	}))
}

type collectorFunc func(chan<- prometheus.Metric)

func (f collectorFunc) Describe(chan<- *prometheus.Desc) {}
func (f collectorFunc) Collect(ch chan<- prometheus.Metric) {
	f(ch)
}

func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return def
}
