package main

import (
	"context"
	"testing"

	"google.golang.org/grpc"

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

// The gateway used to average each node's own mean, which weights a node that evicted
// twice the same as one that evicted ten thousand times. Two nodes, wildly different
// eviction counts: the unweighted answer is 5050 and the true cluster mean is 102.
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
