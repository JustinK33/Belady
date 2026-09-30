package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/grpcx"
	"github.com/JustinK33/Belady/internal/obs"
)

// The HTTP surface makes the cache usable without generating gRPC stubs. It is a
// thin adapter over the same server methods gRPC calls, so routing, the inflight
// ceiling and the metrics are shared rather than reimplemented. Only the gateway
// serves it; cache nodes stay gRPC-only.

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

// serveHTTP runs the REST surface on addr until ctx is cancelled.
func serveHTTP(ctx context.Context, addr string, api *httpAPI) error {
	api.log.Info("http api listening", "addr", addr, "timeout", api.timeout.String())
	return obs.ServeHTTP(ctx, &http.Server{ //nolint:gosec // G112: ServeHTTP sets ReadHeaderTimeout
		Addr:    addr,
		Handler: api.handler(),
		// Room for a full-size body to arrive, plus the upstream call.
		WriteTimeout: 2*api.timeout + 5*time.Second,
	})
}

// handler is the routed and authenticated API. {key...} rather than {key} so a
// slash-separated key like user/42/profile is one key and not a 404.
func (a *httpAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/keys/{key...}", a.get)
	mux.HandleFunc("PUT /v1/keys/{key...}", a.put)
	mux.HandleFunc("DELETE /v1/keys/{key...}", a.delete)
	mux.HandleFunc("GET /v1/stats", a.stats)
	return a.authenticated(mux)
}

// authenticated checks a bearer token in constant time, so response timing does
// not leak how much of a guess was right. The scheme is case-insensitive per
// RFC 7235.
func (a *httpAPI) authenticated(next http.Handler) http.Handler {
	prefix := "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if len(got) <= len(prefix) || !strings.EqualFold(got[:len(prefix)], prefix) ||
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
		if d < time.Second || d/time.Second > math.MaxUint32 {
			writeError(w, http.StatusBadRequest, "ttl must be between 1s and 2^32-1 seconds")
			return
		}
		ttlSeconds = uint32(d / time.Second)
	}

	// MaxBytesReader rather than a length check: a chunked request has no
	// Content-Length to check, and this bounds memory either way.
	value, err := io.ReadAll(http.MaxBytesReader(w, r.Body, grpcx.MaxRecvBytes))
	if tooBig := new(http.MaxBytesError); errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "value exceeds the maximum message size")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body")
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
	case codes.Canceled:
		// The client went away; nobody reads this response, and it is not an error
		// worth logging.
		code = statusClientClosed
	}
	if code == http.StatusInternalServerError {
		a.log.Error("http request failed", "code", st.Code().String(), "err", st.Message())
	}
	writeError(w, code, st.Message())
}

// statusClientClosed is nginx's 499, the conventional code for a request the
// client abandoned.
const statusClientClosed = 499

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func ratio(num, den uint64) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}
