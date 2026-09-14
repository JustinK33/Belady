package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/grpcx"
)

// The HTTP surface exists so the cache is usable without generating gRPC stubs
// first, which is the difference between "a service" and "a service you can try".
// It is a thin adapter: every request goes through the same server methods gRPC
// calls, so routing, single-flight, the inflight ceiling and the metrics are shared
// rather than reimplemented.
//
// Gateway only, deliberately. Cache nodes stay gRPC-only so the one reachable
// surface is also the one that owns rate limiting.

// errNoToken is returned rather than started-and-unauthenticated. An open HTTP port
// in front of a cache is a data leak, and defaulting to no auth is how that happens
// by accident.
var errNoToken = errors.New("HTTP_ADDR is set but HTTP_AUTH_TOKEN is empty: refusing to serve an unauthenticated cache")

type httpAPI struct {
	srv   *server
	log   *slog.Logger
	token []byte

	timeout time.Duration
}

// serveHTTP runs the REST surface until ctx is cancelled. addr empty means off.
func serveHTTP(ctx context.Context, addr, token string, timeout time.Duration, srv *server, log *slog.Logger) error {
	if addr == "" {
		return nil
	}
	if token == "" {
		return errNoToken
	}

	api := &httpAPI{srv: srv, token: []byte(token), timeout: timeout, log: log}

	// {key...} rather than {key} so a slash-separated key like user/42/profile is one
	// key and not a 404.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/keys/{key...}", api.get)
	mux.HandleFunc("PUT /v1/keys/{key...}", api.put)
	mux.HandleFunc("DELETE /v1/keys/{key...}", api.delete)
	mux.HandleFunc("GET /v1/stats", api.stats)

	h := &http.Server{
		Addr:              addr,
		Handler:           api.authenticated(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The shutdown context below is deliberately not derived from ctx: ctx is already
	// cancelled by the time this goroutine wakes, and a cancelled context gives
	// Shutdown no grace period at all.
	go func() { //nolint:gosec // G118, see above
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.Shutdown(shutdown)
	}()

	log.Info("http api listening", "addr", addr, "timeout", timeout.String())
	if err := h.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// authenticated checks a bearer token in constant time. A plain == would leak the
// token's prefix through timing, which is cheap to avoid and awkward to explain.
func (a *httpAPI) authenticated(next http.Handler) http.Handler {
	prefix := "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if len(got) <= len(prefix) || got[:len(prefix)] != prefix ||
			subtle.ConstantTimeCompare([]byte(got[len(prefix):]), a.token) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="belady"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// context bounds the upstream work. Without it a client that walks away leaves the
// gateway holding an inflight slot until the cache node answers.
func (a *httpAPI) context(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), a.timeout)
}

func (a *httpAPI) get(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key must not be empty")
		return
	}
	ctx, cancel := a.context(r)
	defer cancel()

	resp, err := a.srv.Get(ctx, &beladyv1.GetRequest{Key: key})
	if err != nil {
		a.writeStatus(w, err)
		return
	}
	if !resp.GetFound() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Belady-Node", resp.GetServedBy())
	w.Header().Set("X-Belady-Source", sourceName(resp.GetSource()))
	_, _ = w.Write(resp.GetValue())
}

func (a *httpAPI) put(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key must not be empty")
		return
	}

	// ttl is a Go duration string: 30s, 5m, 1h. Rounded to whole seconds because that
	// is the wire type, and rejected rather than truncated when it rounds to zero, so
	// "?ttl=100ms" cannot silently mean "no TTL at all".
	var ttlSeconds uint32
	if raw := r.URL.Query().Get("ttl"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ttl must be a duration such as 30s or 5m")
			return
		}
		if d < time.Second {
			writeError(w, http.StatusBadRequest, "ttl must be at least 1s")
			return
		}
		ttlSeconds = uint32(d / time.Second)
	}

	// MaxBytesReader rather than a length check: a chunked request has no
	// Content-Length to check, and this bounds memory either way.
	value, err := io.ReadAll(http.MaxBytesReader(w, r.Body, grpcx.MaxRecvBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "value exceeds the maximum message size")
		return
	}

	ctx, cancel := a.context(r)
	defer cancel()

	resp, err := a.srv.Put(ctx, &beladyv1.PutRequest{Key: key, Value: value, TtlSeconds: ttlSeconds})
	if err != nil {
		a.writeStatus(w, err)
		return
	}
	// 200 with admitted=false, not an error: the write was handled correctly and the
	// admission policy declined, which the client may well not care about.
	writeJSON(w, http.StatusOK, map[string]any{
		"admitted": resp.GetAdmitted(),
		"node":     resp.GetServedBy(),
	})
}

func (a *httpAPI) delete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key must not be empty")
		return
	}
	ctx, cancel := a.context(r)
	defer cancel()

	resp, err := a.srv.Delete(ctx, &beladyv1.DeleteRequest{Key: key})
	if err != nil {
		a.writeStatus(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"existed": resp.GetExisted()})
}

func (a *httpAPI) stats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := a.context(r)
	defer cancel()

	st, err := a.srv.Stats(ctx, &beladyv1.StatsRequest{})
	if err != nil {
		a.writeStatus(w, err)
		return
	}
	hits, misses := st.GetHits(), st.GetMisses()
	writeJSON(w, http.StatusOK, map[string]any{
		"policy":           st.GetPolicy(),
		"model_version":    st.GetModelVersion(),
		"hits":             hits,
		"misses":           misses,
		"object_hit_ratio": ratio(hits, hits+misses),
		"byte_hit_ratio":   ratio(st.GetHitBytes(), st.GetHitBytes()+st.GetMissBytes()),
		"objects":          st.GetObjects(),
		"bytes_used":       st.GetBytesUsed(),
		"bytes_capacity":   st.GetBytesCapacity(),
		"evictions":        st.GetEvictions(),
		"expirations":      st.GetExpirations(),
		"rejections":       st.GetRejections(),
		"evict_ns_mean":    st.GetEvictNsMean(),
		"evict_ns":         st.GetEvictNs(),
		"evict_sample":     st.GetEvictSample(),
	})
}

func ratio(num, den uint64) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func sourceName(s beladyv1.Source) string {
	switch s {
	case beladyv1.Source_SOURCE_CACHE:
		return "cache"
	case beladyv1.Source_SOURCE_ORIGIN:
		return "origin"
	default:
		return "unknown"
	}
}

// writeStatus maps a gRPC error onto the HTTP status a client can act on. Only the
// codes these handlers can actually produce are listed; anything else is a 500,
// because guessing a friendlier code for an unexpected failure hides it.
func (a *httpAPI) writeStatus(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	code := http.StatusInternalServerError
	switch st.Code() {
	case codes.InvalidArgument:
		code = http.StatusBadRequest
	case codes.NotFound:
		code = http.StatusNotFound
	case codes.ResourceExhausted:
		code = http.StatusTooManyRequests
	case codes.Unavailable:
		code = http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		code = http.StatusGatewayTimeout
	}
	if code == http.StatusInternalServerError {
		a.log.Error("http request failed", "code", st.Code().String(), "err", st.Message())
	}
	writeError(w, code, st.Message())
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
