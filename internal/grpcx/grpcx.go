// Package grpcx holds the gRPC server and client construction every service
// shares, so that limits and interceptors are set in one place rather than
// rediscovered per binary.
package grpcx

import (
	"context"
	"log/slog"
	"net"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/JustinK33/newproj/internal/obs"
)

const (
	// MaxRecvBytes caps an inbound message. Cached values are small by design and
	// model chunks are streamed, so anything larger is a bug or an attack.
	MaxRecvBytes = 8 << 20

	// MaxConcurrentStreams bounds in-flight requests per connection. Without it a
	// single client can queue unbounded work and turn a latency problem into an
	// out-of-memory one.
	MaxConcurrentStreams = 4096
)

var (
	rpcDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "belady_rpc_duration_seconds",
		Help:    "Server-side RPC latency.",
		Buckets: obs.LatencyBuckets,
	}, []string{"method", "code"})

	rpcPanics = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "belady_rpc_panics_total",
		Help: "Handler panics recovered by the interceptor.",
	})
)

func init() {
	obs.Registry.MustRegister(rpcDuration, rpcPanics)
}

func NewServer(extra ...grpc.ServerOption) *grpc.Server {
	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(MaxRecvBytes),
		grpc.MaxConcurrentStreams(MaxConcurrentStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// Detect a peer that vanished without a FIN, so its streams and buffers
			// are not held forever.
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			// Reject clients that ping more aggressively than this rather than
			// letting them use pings as a load amplifier.
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(recoverUnary, measureUnary),
	}
	return grpc.NewServer(append(opts, extra...)...)
}

// Serve runs a gRPC server until ctx is cancelled, then drains in-flight RPCs.
func Serve(ctx context.Context, addr string, srv *grpc.Server) error {
	healthpb.RegisterHealthServer(srv, health.NewServer())

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		// GracefulStop waits for in-flight RPCs. A hard Stop after a grace period
		// keeps a stuck handler from blocking shutdown forever.
		go func() {
			time.Sleep(10 * time.Second)
			srv.Stop()
		}()
		srv.GracefulStop()
		close(done)
	}()

	slog.Info("grpc listening", "addr", addr)
	if err := srv.Serve(lis); err != nil {
		return err
	}
	<-done
	return nil
}

// Dial returns a lazily connected client. It does not block: grpc-go connects in
// the background and buffers the first RPC, which is why the services need no
// startup ordering in Compose.
func Dial(target string) (*grpc.ClientConn, error) {
	return grpc.NewClient(target,
		// ponytail: insecure transport. mTLS belongs here, behind a config switch;
		// see docs/06-security.md. Everything in this repo runs on one host.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxRecvBytes)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

// recoverUnary converts a handler panic into an Internal error. One malformed
// request should cost one response, not the whole process and every cached object
// in it.
func recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if p := recover(); p != nil {
			rpcPanics.Inc()
			slog.Error("panic in handler", "method", info.FullMethod, "panic", p, "stack", string(debug.Stack()))
			err = status.Errorf(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}

func measureUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	rpcDuration.WithLabelValues(info.FullMethod, status.Code(err).String()).Observe(time.Since(start).Seconds())
	return resp, err
}
