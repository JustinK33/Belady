package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	beladyv1 "github.com/JustinK33/newproj/gen/belady/v1"
	"github.com/JustinK33/newproj/internal/hashring"
)

// fakeNode is a cache node reduced to a map, so these tests exercise the HTTP
// adapter and the routing in front of it rather than the store.
type fakeNode struct {
	beladyv1.CacheClient
	values map[string][]byte
	ttls   map[string]uint32
}

func (f *fakeNode) Get(_ context.Context, in *beladyv1.GetRequest, _ ...grpc.CallOption) (*beladyv1.GetResponse, error) {
	v, ok := f.values[in.GetKey()]
	if !ok {
		return &beladyv1.GetResponse{Found: false, ServedBy: "fake"}, nil
	}
	return &beladyv1.GetResponse{Value: v, Found: true, Source: beladyv1.Source_SOURCE_CACHE, ServedBy: "fake"}, nil
}

func (f *fakeNode) Put(_ context.Context, in *beladyv1.PutRequest, _ ...grpc.CallOption) (*beladyv1.PutResponse, error) {
	f.values[in.GetKey()] = in.GetValue()
	f.ttls[in.GetKey()] = in.GetTtlSeconds()
	return &beladyv1.PutResponse{Admitted: true, ServedBy: "fake"}, nil
}

func (f *fakeNode) Delete(_ context.Context, in *beladyv1.DeleteRequest, _ ...grpc.CallOption) (*beladyv1.DeleteResponse, error) {
	_, existed := f.values[in.GetKey()]
	delete(f.values, in.GetKey())
	return &beladyv1.DeleteResponse{Existed: existed}, nil
}

func (f *fakeNode) Stats(context.Context, *beladyv1.StatsRequest, ...grpc.CallOption) (*beladyv1.StatsResponse, error) {
	return &beladyv1.StatsResponse{Policy: "s3fifo", Hits: 3, Misses: 1}, nil
}

const testToken = "test-token"

func newTestAPI(t *testing.T) (http.Handler, *fakeNode) {
	t.Helper()
	node := &fakeNode{values: map[string][]byte{}, ttls: map[string]uint32{}}

	srv := &server{
		ring:     hashring.New(64, 1.25),
		nodes:    map[string]beladyv1.CacheClient{"fake": node},
		inflight: make(chan struct{}, 8),
	}
	srv.ring.Add("fake")

	api := &httpAPI{
		srv:     srv,
		token:   []byte(testToken),
		timeout: time.Second,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/keys/{key...}", api.get)
	mux.HandleFunc("PUT /v1/keys/{key...}", api.put)
	mux.HandleFunc("DELETE /v1/keys/{key...}", api.delete)
	mux.HandleFunc("GET /v1/stats", api.stats)
	return api.authenticated(mux), node
}

func do(t *testing.T, h http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTPRequiresABearerToken(t *testing.T) {
	h, _ := newTestAPI(t)

	for _, tc := range []struct{ name, token string }{
		{"no header", ""},
		{"wrong token", "not-the-token"},
		{"prefix of the real token", testToken[:4]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := do(t, h, http.MethodGet, "/v1/keys/a", tc.token, "").Code; got != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", got)
			}
		})
	}
}

func TestHTTPRoundTrip(t *testing.T) {
	h, node := newTestAPI(t)

	if got := do(t, h, http.MethodGet, "/v1/keys/absent", testToken, "").Code; got != http.StatusNotFound {
		t.Errorf("missing key returned %d, want 404", got)
	}

	if got := do(t, h, http.MethodPut, "/v1/keys/greeting", testToken, "hello").Code; got != http.StatusOK {
		t.Fatalf("put returned %d", got)
	}

	rec := do(t, h, http.MethodGet, "/v1/keys/greeting", testToken, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("get returned %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Belady-Source"); got != "cache" {
		t.Errorf("X-Belady-Source = %q, want cache", got)
	}

	if got := do(t, h, http.MethodDelete, "/v1/keys/greeting", testToken, "").Code; got != http.StatusOK {
		t.Errorf("delete returned %d", got)
	}
	if _, still := node.values["greeting"]; still {
		t.Error("the key survived a delete")
	}
}

// TestHTTPKeysMayContainSlashes covers the {key...} wildcard: with a plain {key} a
// namespaced key is a 404 rather than a lookup, which is the kind of thing that only
// shows up once someone tries to use it.
func TestHTTPKeysMayContainSlashes(t *testing.T) {
	h, node := newTestAPI(t)

	if got := do(t, h, http.MethodPut, "/v1/keys/user/42/profile", testToken, "{}").Code; got != http.StatusOK {
		t.Fatalf("put returned %d", got)
	}
	if _, ok := node.values["user/42/profile"]; !ok {
		t.Fatalf("stored keys are %v, want user/42/profile", node.values)
	}
}

func TestHTTPTTLParsing(t *testing.T) {
	h, node := newTestAPI(t)

	if got := do(t, h, http.MethodPut, "/v1/keys/a?ttl=90s", testToken, "v").Code; got != http.StatusOK {
		t.Fatalf("valid ttl returned %d", got)
	}
	if node.ttls["a"] != 90 {
		t.Errorf("ttl_seconds = %d, want 90", node.ttls["a"])
	}

	if got := do(t, h, http.MethodPut, "/v1/keys/b?ttl=5m", testToken, "v").Code; got != http.StatusOK {
		t.Fatalf("valid ttl returned %d", got)
	}
	if node.ttls["b"] != 300 {
		t.Errorf("ttl_seconds = %d, want 300", node.ttls["b"])
	}

	// Rejected rather than truncated to zero, which would mean "no TTL" and cache the
	// value forever after the client asked for the opposite.
	for _, bad := range []string{"ttl=100ms", "ttl=soon", "ttl=0s"} {
		if got := do(t, h, http.MethodPut, "/v1/keys/c?"+bad, testToken, "v").Code; got != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", bad, got)
		}
	}
}

func TestHTTPStats(t *testing.T) {
	h, _ := newTestAPI(t)

	rec := do(t, h, http.MethodGet, "/v1/stats", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats returned %d", rec.Code)
	}
	// 3 hits and 1 miss from the fake node.
	if body := rec.Body.String(); !strings.Contains(body, `"object_hit_ratio":0.75`) {
		t.Errorf("stats body = %s", body)
	}
}
