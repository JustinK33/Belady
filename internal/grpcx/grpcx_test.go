package grpcx

import (
	"context"
	"net"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

// A panicking stream handler must come back as Internal, and still be measured.
func TestStreamPanicIsRecoveredAndMeasured(t *testing.T) {
	const method = "/test.Svc/Boom"
	srv := NewServer(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error { panic("boom") }))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	err = conn.Invoke(context.Background(), method, &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("err = %v, want Internal", err)
	}
	var m dto.Metric
	if err := rpcDuration.WithLabelValues(method, codes.Internal.String()).(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	if n := m.GetHistogram().GetSampleCount(); n != 1 {
		t.Fatalf("observed the panicked call %d times, want 1", n)
	}
}
