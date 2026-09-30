package main

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/hashring"
)

// statsNode answers Stats with a canned response and nothing else. Stats is the one
// gateway method that combines numbers rather than forwarding them, so it is the one
// that can be wrong arithmetically.
type statsNode struct {
	beladyv1.CacheClient
	resp *beladyv1.StatsResponse
}

func (s *statsNode) Stats(context.Context, *beladyv1.StatsRequest, ...grpc.CallOption) (*beladyv1.StatsResponse, error) {
	return s.resp, nil
}

// Averaging each node's own mean would weight a node that evicted twice the same as
// one that evicted ten thousand times. Two nodes, wildly different eviction counts:
// the unweighted answer is 5050 and the true cluster mean is 102.
func TestStatsWeightsEvictionCostByEvictionCount(t *testing.T) {
	quiet := &beladyv1.StatsResponse{
		Policy: "lrb", Evictions: 2,
		EvictNs: 20000, EvictSample: 2, EvictNsMean: 10000,
	}
	busy := &beladyv1.StatsResponse{
		Policy: "lrb", Evictions: 10000,
		EvictNs: 1000000, EvictSample: 10000, EvictNsMean: 100,
	}

	srv := &server{
		ring: hashring.New(64, 1.25),
		nodes: map[string]beladyv1.CacheClient{
			"quiet": &statsNode{resp: quiet},
			"busy":  &statsNode{resp: busy},
		},
		inflight: make(chan struct{}, 8),
	}

	got, err := srv.Stats(context.Background(), &beladyv1.StatsRequest{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if want := uint64(1020000); got.GetEvictNs() != want {
		t.Errorf("evict_ns = %d, want %d", got.GetEvictNs(), want)
	}
	if want := uint64(10002); got.GetEvictSample() != want {
		t.Errorf("evict_sample = %d, want %d", got.GetEvictSample(), want)
	}
	if want := uint64(101); got.GetEvictNsMean() != want {
		t.Errorf("evict_ns_mean = %d, want %d (unweighted would give 5050)", got.GetEvictNsMean(), want)
	}
}

// A cluster with no evictions must report zero rather than dividing by zero.
func TestStatsWithNoEvictions(t *testing.T) {
	srv := &server{
		ring:     hashring.New(64, 1.25),
		nodes:    map[string]beladyv1.CacheClient{"a": &statsNode{resp: &beladyv1.StatsResponse{Policy: "lru"}}},
		inflight: make(chan struct{}, 8),
	}
	got, err := srv.Stats(context.Background(), &beladyv1.StatsRequest{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.GetEvictNsMean() != 0 {
		t.Errorf("evict_ns_mean = %d, want 0", got.GetEvictNsMean())
	}
}

func TestStatsReportsMixedModelVersions(t *testing.T) {
	srv := &server{
		inflight: make(chan struct{}, 2),
		nodes: map[string]beladyv1.CacheClient{
			"a": &statsNode{resp: &beladyv1.StatsResponse{Policy: "lrb", ModelVersion: "v1"}},
			"b": &statsNode{resp: &beladyv1.StatsResponse{Policy: "lrb", ModelVersion: "v2"}},
		},
	}
	st, err := srv.Stats(context.Background(), &beladyv1.StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.GetPolicy() != "lrb" || st.GetModelVersion() != "mixed" {
		t.Fatalf("policy %q, model %q, want lrb and mixed", st.GetPolicy(), st.GetModelVersion())
	}
}

type deleteNode struct {
	beladyv1.CacheClient
	err error
}

func (d *deleteNode) Delete(context.Context, *beladyv1.DeleteRequest, ...grpc.CallOption) (*beladyv1.DeleteResponse, error) {
	return &beladyv1.DeleteResponse{}, d.err
}

// A node timing out is a 504 at the HTTP edge, not a 503, so the code has to
// survive the fan-out.
func TestDeleteKeepsTheNodeErrorCode(t *testing.T) {
	srv := &server{
		inflight: make(chan struct{}, 2),
		nodes: map[string]beladyv1.CacheClient{
			"a": &deleteNode{},
			"b": &deleteNode{err: status.Error(codes.DeadlineExceeded, "slow")},
		},
	}
	_, err := srv.Delete(context.Background(), &beladyv1.DeleteRequest{Key: "k"})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if _, err := srv.Delete(context.Background(), &beladyv1.DeleteRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty key: err = %v, want InvalidArgument", err)
	}
}

// A client that hangs up while queued is not evidence of overload.
func TestAdmitTellsCancelFromCapacity(t *testing.T) {
	srv := &server{inflight: make(chan struct{}, 1)}
	srv.inflight <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.admit(ctx); status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled: err = %v, want Canceled", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := srv.admit(ctx); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("deadline: err = %v, want ResourceExhausted", err)
	}
}
