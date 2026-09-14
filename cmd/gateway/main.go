// Command gateway is the client-facing edge. It owns routing, admission of
// concurrency, and nothing else: no cache, no policy, no state worth losing.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/config"
	"github.com/JustinK33/Belady/internal/grpcx"
	"github.com/JustinK33/Belady/internal/hashring"
	"github.com/JustinK33/Belady/internal/obs"
)

var routed = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "belady_gateway_routed_total",
	Help: "Requests routed, by destination node.",
}, []string{"node"})

func init() {
	obs.Registry.MustRegister(routed)
}

type server struct {
	beladyv1.UnimplementedCacheServer

	ring  *hashring.Ring
	nodes map[string]beladyv1.CacheClient

	// inflight bounds concurrent upstream work. A gateway with no ceiling converts
	// a downstream slowdown into unbounded memory growth here, which is how a
	// latency incident becomes an outage.
	// ponytail: a semaphore, not a token bucket. Per-tenant QPS limiting belongs at
	// the edge proxy that already knows who the tenant is.
	inflight chan struct{}
}

// route picks a node, charges its load, and returns a release function. Load is
// only meaningful if it is released on every path, so the caller defers this.
func (s *server) route(key string) (beladyv1.CacheClient, string, func(), error) {
	name, err := s.ring.Pick(key)
	if err != nil {
		return nil, "", nil, status.Error(codes.Unavailable, "no cache nodes configured")
	}
	client, ok := s.nodes[name]
	if !ok {
		s.ring.Done(name)
		return nil, "", nil, status.Errorf(codes.Internal, "ring returned unknown node %q", name)
	}
	routed.WithLabelValues(name).Inc()
	return client, name, func() { s.ring.Done(name) }, nil
}

func (s *server) admit(ctx context.Context) (func(), error) {
	select {
	case s.inflight <- struct{}{}:
		return func() { <-s.inflight }, nil
	case <-ctx.Done():
		// The caller's deadline expired while queued. Reporting this as a distinct
		// code is what lets a dashboard tell overload apart from slowness.
		return nil, status.Error(codes.ResourceExhausted, "gateway at capacity")
	}
}

func (s *server) Get(ctx context.Context, req *beladyv1.GetRequest) (*beladyv1.GetResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key must not be empty")
	}
	release, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	client, _, done, err := s.route(req.GetKey())
	if err != nil {
		return nil, err
	}
	defer done()

	return client.Get(ctx, req)
}

func (s *server) Put(ctx context.Context, req *beladyv1.PutRequest) (*beladyv1.PutResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key must not be empty")
	}
	release, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	client, _, done, err := s.route(req.GetKey())
	if err != nil {
		return nil, err
	}
	defer done()

	return client.Put(ctx, req)
}

func (s *server) Delete(ctx context.Context, req *beladyv1.DeleteRequest) (*beladyv1.DeleteResponse, error) {
	// Bounded loads mean a key is not guaranteed to live on exactly one node, so a
	// delete has to reach all of them. Cheap, and the alternative is a stale value
	// resurfacing when load shifts.
	existed := false
	for name, client := range s.nodes {
		resp, err := client.Delete(ctx, req)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "delete on %s failed: %v", name, err)
		}
		existed = existed || resp.GetExisted()
	}
	return &beladyv1.DeleteResponse{Existed: existed}, nil
}

// Stats sums the cluster. Counters add cleanly, and the eviction-time mean is
// recomputed from the summed numerator and denominator rather than averaged, so a
// node that evicted twice does not count as much as one that evicted ten thousand
// times. It is still a mean, not a percentile.
func (s *server) Stats(ctx context.Context, req *beladyv1.StatsRequest) (*beladyv1.StatsResponse, error) {
	out := &beladyv1.StatsResponse{NodeId: "gateway"}

	for name, client := range s.nodes {
		st, err := client.Stats(ctx, req)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "stats from %s failed: %v", name, err)
		}
		switch {
		case out.Policy == "":
			out.Policy = st.GetPolicy()
			out.ModelVersion = st.GetModelVersion()
		case out.Policy != st.GetPolicy():
			// A mixed cluster makes every hit-ratio comparison meaningless, so say so
			// loudly rather than reporting a blended number.
			out.Policy = "mixed"
		}

		out.Hits += st.GetHits()
		out.Misses += st.GetMisses()
		out.HitBytes += st.GetHitBytes()
		out.MissBytes += st.GetMissBytes()
		out.Admissions += st.GetAdmissions()
		out.Rejections += st.GetRejections()
		out.Evictions += st.GetEvictions()
		out.Expirations += st.GetExpirations()
		out.Objects += st.GetObjects()
		out.BytesUsed += st.GetBytesUsed()
		out.BytesCapacity += st.GetBytesCapacity()
		out.TraceSampled += st.GetTraceSampled()
		out.TraceDropped += st.GetTraceDropped()
		out.EvictNs += st.GetEvictNs()
		out.EvictSample += st.GetEvictSample()
	}
	// Summing the raw pair and dividing once gives the true cluster mean. Averaging
	// each node's mean, which is what this used to do, weights a node that evicted
	// twice the same as one that evicted ten thousand times.
	if out.EvictSample > 0 {
		out.EvictNsMean = out.EvictNs / out.EvictSample
	}
	return out, nil
}

func main() {
	log := obs.Init("gateway")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	addrs := config.Strings("CACHE_NODES", nil)
	if len(addrs) == 0 {
		log.Error("CACHE_NODES is required, as a comma-separated list of host:port")
		os.Exit(2)
	}

	srv := &server{
		ring:     hashring.New(config.Int("RING_REPLICAS", 256), config.Float("RING_LOAD_FACTOR", 1.25)),
		nodes:    make(map[string]beladyv1.CacheClient, len(addrs)),
		inflight: make(chan struct{}, config.Int("MAX_INFLIGHT", 4096)),
	}

	conns := make([]*grpc.ClientConn, 0, len(addrs))
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	for _, addr := range addrs {
		conn, err := grpcx.Dial(addr)
		if err != nil {
			log.Error("dial failed", "addr", addr, "err", err)
			os.Exit(2)
		}
		conns = append(conns, conn)
		srv.nodes[addr] = beladyv1.NewCacheClient(conn)
		srv.ring.Add(addr)
	}
	log.Info("gateway configured", "nodes", srv.ring.Members(), "max_inflight", cap(srv.inflight))

	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "belady_gateway_inflight",
		Help: "Requests currently held by the gateway.",
	}, func() float64 { return float64(len(srv.inflight)) }))

	go func() {
		if err := obs.Serve(ctx, config.String("DEBUG_ADDR", ":9090")); err != nil {
			log.Error("debug endpoint failed", "err", err)
		}
	}()

	// The REST surface is off unless HTTP_ADDR is set, and a bad HTTP config is fatal
	// rather than logged: starting without the API someone asked for is worse than
	// not starting.
	httpAddr := config.String("HTTP_ADDR", "")
	httpTimeout := config.Duration("HTTP_TIMEOUT", 5*time.Second)
	if httpAddr != "" && config.String("HTTP_AUTH_TOKEN", "") == "" {
		log.Error("bad configuration", "err", errNoToken)
		os.Exit(2)
	}
	go func() {
		if err := serveHTTP(ctx, httpAddr, config.String("HTTP_AUTH_TOKEN", ""), httpTimeout, srv, log); err != nil {
			log.Error("http api failed", "err", err)
		}
	}()

	g := grpcx.NewServer()
	beladyv1.RegisterCacheServer(g, srv)
	if err := grpcx.Serve(ctx, config.String("GRPC_ADDR", ":8080"), g); err != nil {
		log.Error("serve failed", "err", err)
	}
}
