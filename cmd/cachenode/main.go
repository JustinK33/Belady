// Command cachenode is the data plane: one shard-set of the cache, one eviction
// policy, one connection to origin.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beladyv1 "github.com/JustinK33/newproj/gen/belady/v1"
	"github.com/JustinK33/newproj/internal/cache"
	"github.com/JustinK33/newproj/internal/config"
	"github.com/JustinK33/newproj/internal/grpcx"
	"github.com/JustinK33/newproj/internal/obs"
	"github.com/JustinK33/newproj/internal/registry"
	"github.com/JustinK33/newproj/internal/trace"
)

type server struct {
	beladyv1.UnimplementedCacheServer

	cache  *cache.Cache
	origin beladyv1.OriginClient

	// models is nil unless the learned policy is configured. Stats reads the live
	// version from it so a hit ratio can never be attributed to a model that was
	// not actually installed.
	models *cache.ModelHolder
	trace  *trace.Recorder

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
	var version string
	if s.models != nil {
		version = s.models.Version()
	}
	var sampled, dropped uint64
	if s.trace != nil {
		sampled, dropped = s.trace.Stats()
	}
	return &beladyv1.StatsResponse{
		NodeId:        s.nodeID,
		Policy:        st.Policy,
		ModelVersion:  version,
		TraceSampled:  sampled,
		TraceDropped:  dropped,
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

// newPolicy maps the configured name to a constructor, returning the model holder
// as well when the learned policy needs one. An unknown name is fatal rather than
// defaulted: silently running LRU while a dashboard claims the learned policy is
// worse than crashing.
func newPolicy(name string) (func(int64) cache.Policy, *cache.ModelHolder, error) {
	sample := config.Int("CACHE_SAMPLE_SIZE", 8)
	switch name {
	case "lru":
		return func(int64) cache.Policy { return cache.NewLRU(sample) }, nil, nil
	case "lfu":
		return func(int64) cache.Policy { return cache.NewLFU(sample) }, nil, nil
	case "s3fifo":
		return cache.NewS3FIFO, nil, nil
	case "lrb":
		// One holder shared by every shard: installing a model is a single pointer
		// store, so a rollout never has to touch a shard lock.
		holder := cache.NewModelHolder()
		return func(int64) cache.Policy { return cache.NewLRB(holder, sample) }, holder, nil
	default:
		return nil, nil, fmt.Errorf("unknown CACHE_POLICY %q: want lru, lfu, s3fifo or lrb", name)
	}
}

// newRecorder builds the access-trace recorder, or returns nil when tracing is off.
//
// Off by default because it writes files: a node should not start filling a disk
// because nobody set a variable. The training pipeline turns it on explicitly.
func newRecorder(nodeID string, shards int, log *slog.Logger) (*trace.Recorder, error) {
	if !config.Bool("TRACE_ENABLED", false) {
		return nil, nil
	}
	return trace.New(trace.Config{
		Dir:    config.String("TRACE_DIR", "/var/lib/belady/traces"),
		NodeID: nodeID,
		Shards: shards,
		// One key in N, not one request in N. See trace.Ring.Push for why that
		// distinction decides whether the labels come out right.
		SampleDenominator: config.Int("TRACE_SAMPLE_DENOMINATOR", 16),
		RingCapacity:      config.Int("TRACE_RING_CAPACITY", 8192),
		SegmentBytes:      config.Bytes("TRACE_SEGMENT_BYTES", 32<<20),
		FlushInterval:     config.Duration("TRACE_FLUSH_INTERVAL", time.Second),
		BatchSize:         config.Int("TRACE_BATCH_SIZE", 4096),
	}, log)
}

func main() {
	log := obs.Init("cachenode")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	policyName := config.String("CACHE_POLICY", "lru")
	mkPolicy, models, err := newPolicy(policyName)
	if err != nil {
		log.Error("bad configuration", "err", err)
		os.Exit(2)
	}

	nodeID := config.String("NODE_ID", hostnameOr("cachenode"))
	shards := cache.RoundShards(config.Int("CACHE_SHARDS", 256))
	recorder, err := newRecorder(nodeID, shards, log)
	if err != nil {
		log.Error("trace recorder init failed", "err", err)
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
		Shards:        shards,
		SampleSize:    config.Int("CACHE_SAMPLE_SIZE", 8),
		Trace:         recorder,
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

	srv := &server{
		cache:  c,
		origin: beladyv1.NewOriginClient(conn),
		models: models,
		trace:  recorder,
		nodeID: nodeID,
	}

	// The learned policy is the only one that needs a model, so the registry
	// connection follows the policy rather than being wired up unconditionally.
	boundary := config.Duration("MODEL_BOUNDARY", 10*time.Minute)
	if models != nil {
		addr := config.String("REGISTRY_ADDR", "")
		if addr == "" {
			// Not fatal: this is exactly the state of a fresh cluster, and running the
			// fallback policy is the correct behaviour. It has to be loud, though,
			// because a benchmark labelled "lrb" that never loaded a model is just LRU.
			log.Warn("policy lrb is configured with no REGISTRY_ADDR, so no model will ever load")
		} else {
			rconn, err := grpcx.Dial(addr)
			if err != nil {
				log.Error("registry dial failed", "addr", addr, "err", err)
				os.Exit(2)
			}
			defer func() { _ = rconn.Close() }()

			go registry.NewWatcher(rconn, models, boundary, log).Run(ctx)
		}
	}

	registerCacheMetrics(c, recorder)
	_, perShard := c.Shape()
	log.Info("cache node configured", "node_id", nodeID, "policy", policyName,
		"capacity", capacity, "shards", shards, "per_shard_bytes", perShard,
		"trace", recorder != nil, "model_boundary", boundary.String())

	if recorder != nil {
		// Run owns the segment files, so it has to finish its final flush before the
		// process exits or the last segment is left as a .partial nobody reads.
		traceDone := make(chan struct{})
		go func() {
			defer close(traceDone)
			if err := recorder.Run(ctx); err != nil {
				log.Error("trace recorder stopped", "err", err)
			}
		}()
		defer func() { <-traceDone }()
	}

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
func registerCacheMetrics(c *cache.Cache, rec *trace.Recorder) {
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

	// Trace counters come from the recorder rather than the store. The dropped
	// counter is the one that matters: it is the only signal that a model was
	// trained on an incomplete view of the traffic.
	written := desc("trace_written_total", "Access records written to a trace segment.")
	dropped := desc("trace_dropped_total", "Access records discarded because a trace ring was full.")

	obs.Registry.MustRegister(collectorFunc(func(ch chan<- prometheus.Metric) {
		st := c.Snapshot()
		for _, s := range specs {
			ch <- prometheus.MustNewConstMetric(s.d, prometheus.GaugeValue, s.val(st))
		}
		if rec != nil {
			w, d := rec.Stats()
			ch <- prometheus.MustNewConstMetric(written, prometheus.CounterValue, float64(w))
			ch <- prometheus.MustNewConstMetric(dropped, prometheus.CounterValue, float64(d))
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
