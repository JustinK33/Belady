package main

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/cache"
)

// slowOrigin answers every fetch with value once release is closed.
type slowOrigin struct {
	release chan struct{}
	value   []byte
}

func (o *slowOrigin) Fetch(ctx context.Context, _ *beladyv1.FetchRequest, _ ...grpc.CallOption) (*beladyv1.FetchResponse, error) {
	select {
	case <-o.release:
		return &beladyv1.FetchResponse{Found: true, Value: o.value}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newTestServer(t *testing.T, origin beladyv1.OriginClient) *server {
	t.Helper()
	c, err := cache.New(cache.Config{NewPolicy: cache.NewS3FIFO, CapacityBytes: 1 << 20, Shards: 1})
	if err != nil {
		t.Fatal(err)
	}
	return &server{cache: c, origin: origin, originTimeout: 5 * time.Second, nodeID: "test"}
}

// The fetch is shared, so the first caller giving up must not fail everyone else
// waiting on the same key.
func TestSharedFetchSurvivesTheFirstCallerCancelling(t *testing.T) {
	origin := &slowOrigin{release: make(chan struct{}), value: []byte("v")}
	s := newTestServer(t, origin)

	first, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.Get(first, &beladyv1.GetRequest{Key: "k"})
		firstDone <- err
	}()
	time.Sleep(20 * time.Millisecond)

	second := make(chan *beladyv1.GetResponse, 1)
	go func() {
		resp, err := s.Get(context.Background(), &beladyv1.GetRequest{Key: "k"})
		if err != nil {
			t.Error(err)
		}
		second <- resp
	}()
	time.Sleep(20 * time.Millisecond)

	cancel()
	if err := <-firstDone; err == nil {
		t.Error("cancelled caller got no error")
	}
	close(origin.release)
	if resp := <-second; !resp.GetFound() || string(resp.GetValue()) != "v" {
		t.Fatalf("second caller got %v", resp)
	}
}

func TestEmptyOriginValueIsFound(t *testing.T) {
	origin := &slowOrigin{release: make(chan struct{})}
	close(origin.release)
	s := newTestServer(t, origin)

	resp, err := s.Get(context.Background(), &beladyv1.GetRequest{Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetFound() {
		t.Fatal("an empty value that exists at origin was reported as not found")
	}
}
