// Command gateway is the client-facing edge. It owns routing, admission of
// concurrency, and nothing else: no cache, no policy, no state worth losing.
package main

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/config"
	"github.com/JustinK33/Belady/internal/grpcx"
	"github.com/JustinK33/Belady/internal/hashring"
	"github.com/JustinK33/Belady/internal/obs"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

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

	// inflight bounds concurrent upstream work, so a downstream slowdown queues
	// here as a ResourceExhausted error rather than as unbounded memory growth.
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
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		// The deadline expired while queued. A distinct code is what lets a dashboard
		// tell overload apart from slowness.
		return nil, status.Error(codes.ResourceExhausted, "gateway at capacity")
	}
}

type keyed interface{ GetKey() string }

// forward sends a single-key request to the node that owns the key.
func forward[Req keyed, Resp any](ctx context.Context, s *server, req Req,
	call func(beladyv1.CacheClient, context.Context, Req, ...grpc.CallOption) (Resp, error),
) (resp Resp, err error) {
	if req.GetKey() == "" {
		return resp, status.Error(codes.InvalidArgument, "key must not be empty")
	}
	release, err := s.admit(ctx)
	if err != nil {
		return resp, err
	}
	defer release()

	client, _, done, err := s.route(req.GetKey())
	if err != nil {
		return resp, err
	}
	defer done()

	return call(client, ctx, req)
}

func (s *server) Get(ctx context.Context, req *beladyv1.GetRequest) (*beladyv1.GetResponse, error) {
	return forward(ctx, s, req, beladyv1.CacheClient.Get)
}

func (s *server) Put(ctx context.Context, req *beladyv1.PutRequest) (*beladyv1.PutResponse, error) {
	return forward(ctx, s, req, beladyv1.CacheClient.Put)
}

// broadcast calls every node in parallel and returns their answers in node-name
// order. The first failure cancels the rest and keeps its gRPC code.
func broadcast[Resp any](ctx context.Context, s *server, op string,
	call func(context.Context, beladyv1.CacheClient) (Resp, error),
) ([]Resp, error) {
	release, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	names := slices.Sorted(maps.Keys(s.nodes))
	out := make([]Resp, len(names))
	g, gctx := errgroup.WithContext(ctx)
	for i, name := range names {
		g.Go(func() error {
			resp, err := call(gctx, s.nodes[name])
			if err != nil {
				return status.Errorf(status.Code(err), "%s on %s failed: %s", op, name, status.Convert(err).Message())
			}
			out[i] = resp
			return nil
		})
	}
	return out, g.Wait()
}

// Delete reaches every node: bounded loads mean a key is not guaranteed to live on
// exactly one, and a stale copy would resurface when load shifts. A failure can
// leave the key deleted on some nodes, so the client should retry.
func (s *server) Delete(ctx context.Context, req *beladyv1.DeleteRequest) (*beladyv1.DeleteResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key must not be empty")
	}
	resps, err := broadcast(ctx, s, "delete", func(ctx context.Context, c beladyv1.CacheClient) (*beladyv1.DeleteResponse, error) {
		return c.Delete(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	existed := false
	for _, r := range resps {
		existed = existed || r.GetExisted()
	}
	return &beladyv1.DeleteResponse{Existed: existed}, nil
}

// Stats sums the cluster. The eviction-time mean is recomputed from the summed
// numerator and denominator rather than averaged, so a node that evicted twice
// does not count as much as one that evicted ten thousand times. It is still a
// mean, not a percentile.
func (s *server) Stats(ctx context.Context, req *beladyv1.StatsRequest) (*beladyv1.StatsResponse, error) {
	resps, err := broadcast(ctx, s, "stats", func(ctx context.Context, c beladyv1.CacheClient) (*beladyv1.StatsResponse, error) {
		return c.Stats(ctx, req)
	})
	if err != nil {
		return nil, err
	}

	out := &beladyv1.StatsResponse{NodeId: "gateway"}
	for i, st := range resps {
		// A mixed cluster makes every hit-ratio comparison meaningless, so say so
		// rather than reporting a blended number.
		if i == 0 {
			out.Policy, out.ModelVersion = st.GetPolicy(), st.GetModelVersion()
		}
		if out.Policy != st.GetPolicy() {
			out.Policy = "mixed"
		}
		if out.ModelVersion != st.GetModelVersion() {
			out.ModelVersion = "mixed"
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
		out.TraceWritten += st.GetTraceWritten()
		out.TraceDropped += st.GetTraceDropped()
		out.EvictNs += st.GetEvictNs()
		out.EvictSample += st.GetEvictSample()
	}
	if out.EvictSample > 0 {
		out.EvictNsMean = out.EvictNs / out.EvictSample
	}
	return out, nil
}

func main() {
	log, ctx, stop := obs.Start("gateway", version)
	defer stop()

	addrs := config.Strings("CACHE_NODES", nil)
	if len(addrs) == 0 {
		obs.Fatal(log, "CACHE_NODES is required, as a comma-separated list of host:port")
	}

	replicas := config.Int("RING_REPLICAS", 256)
	factor := config.Float("RING_LOAD_FACTOR", 1.25)
	maxInflight := config.Int("MAX_INFLIGHT", 4096)
	switch {
	case replicas < 1:
		obs.Fatal(log, "RING_REPLICAS must be at least 1", "value", replicas)
	case factor <= 1:
		obs.Fatal(log, "RING_LOAD_FACTOR must be above 1", "value", factor)
	case maxInflight < 1:
		obs.Fatal(log, "MAX_INFLIGHT must be at least 1", "value", maxInflight)
	}

	srv := &server{
		ring:     hashring.New(replicas, factor),
		nodes:    make(map[string]beladyv1.CacheClient, len(addrs)),
		inflight: make(chan struct{}, maxInflight),
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
			obs.Fatal(log, "dial failed", "addr", addr, "err", err)
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

	// The REST surface is off unless HTTP_ADDR is set. A failure to serve it stops
	// the process: running without the API someone asked for is worse than not
	// running.
	httpAddr := config.String("HTTP_ADDR", "")
	token := config.String("HTTP_AUTH_TOKEN", "")
	if httpAddr != "" && token == "" {
		obs.Fatal(log, "bad configuration", "err", errNoToken)
	}
	var httpErr error
	httpDone := make(chan struct{})
	go func() {
		defer close(httpDone)
		if httpAddr == "" {
			return
		}
		api := &httpAPI{srv: srv, token: []byte(token), timeout: config.Duration("HTTP_TIMEOUT", 5*time.Second), log: log}
		if httpErr = serveHTTP(ctx, httpAddr, api); httpErr != nil {
			log.Error("http api failed", "err", httpErr)
			stop()
		}
	}()

	g := grpcx.NewServer()
	beladyv1.RegisterCacheServer(g, srv)
	err := grpcx.Serve(ctx, config.String("GRPC_ADDR", ":8080"), g)
	stop()
	if err != nil {
		log.Error("serve failed", "err", err)
	}
	// Both servers have drained before the deferred node connections close.
	<-httpDone
	if err != nil || httpErr != nil {
		os.Exit(1)
	}
}
