package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/cache"
	"github.com/JustinK33/Belady/internal/model"
	"github.com/JustinK33/Belady/internal/obs"
)

var (
	modelLoads = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "belady_model_loads_total",
		Help: "Models successfully installed into the eviction policy.",
	})
	modelFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "belady_model_load_failures_total",
		Help: "Models the node fetched and then refused.",
	})
)

func init() { obs.Registry.MustRegister(modelLoads, modelFailures) }

// watcher keeps the node's model current.
//
// It runs entirely off the request path: parsing a LightGBM dump into the flat tree
// arrays takes milliseconds, and the swap into the holder is one atomic pointer
// store, so an eviction happening at that moment finishes against the old model
// rather than waiting.
type Watcher struct {
	client   beladyv1.RegistryClient
	holder   *cache.ModelHolder
	log      *slog.Logger
	boundary time.Duration
}

// NewWatcher returns a Watcher that installs models from the registry at addr's
// connection into holder. boundary is the Relaxed Belady boundary this node is
// configured for; a model trained against a different one is refused.
func NewWatcher(conn grpc.ClientConnInterface, holder *cache.ModelHolder, boundary time.Duration, log *slog.Logger) *Watcher {
	return &Watcher{
		client:   beladyv1.NewRegistryClient(conn),
		holder:   holder,
		log:      log,
		boundary: boundary,
	}
}

// Run watches until ctx is cancelled, reconnecting with backoff.
//
// A registry outage must not affect serving: the node keeps evicting with whatever
// model it already has, or with its fallback policy if it never got one. So every
// failure here is logged and retried rather than returned.
func (w *Watcher) Run(ctx context.Context) {
	const minBackoff, maxBackoff = time.Second, 30 * time.Second
	backoff := minBackoff

	for ctx.Err() == nil {
		started := time.Now()
		err := w.watch(ctx) // Only ever returns because the stream ended, so err is never nil.
		if ctx.Err() != nil {
			return
		}
		// A stream that stayed up is not a failing endpoint. Without this the backoff
		// only ever grows, so a node that reconnects once an hour ends up waiting the
		// maximum for a registry that is perfectly healthy.
		if time.Since(started) >= maxBackoff {
			backoff = minBackoff
		}
		w.log.Warn("model watch dropped, retrying", "err", err, "in", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// watch runs one stream to completion. It has no success case: it returns only when
// the stream breaks or ctx is cancelled, so the error it returns is never nil.
func (w *Watcher) watch(ctx context.Context) error {
	// since is the installed version, so a reconnect does not re-download it and a
	// node that restarts is caught up immediately by the registry.
	stream, err := w.client.WatchModels(ctx, &beladyv1.WatchModelsRequest{
		SinceVersion: w.holder.Version(),
	})
	if err != nil {
		return err
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := w.install(ctx, event.GetMeta()); err != nil {
			modelFailures.Inc()
			// A rejected model is a bad model, not a bad connection. Log it loudly and
			// keep watching: the next training run may well be fine, and tearing down
			// the stream would only delay that.
			w.log.Error("refused model", "version", event.GetMeta().GetVersion(), "err", err)
		}
	}
}

func (w *Watcher) install(ctx context.Context, meta *beladyv1.ModelMeta) error {
	// Check what the registry told us before spending a download on it.
	if err := w.compatible(meta); err != nil {
		return err
	}

	body, served, err := w.fetch(ctx, meta.GetVersion())
	if err != nil {
		return err
	}
	// The served metadata is authoritative for what actually arrived; the watch event
	// only announced that something exists.
	if err := w.compatible(served); err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != served.GetSha256() {
		return fmt.Errorf("digest mismatch: registry declared %s, received %s", served.GetSha256(), got)
	}

	start := time.Now()
	m, err := model.Load(bytes.NewReader(body))
	if err != nil {
		return err
	}
	// Store re-checks the feature count against this build's layout. That check and
	// the one in compatible are not redundant: this one is against what the model
	// file actually contains, which is the only claim that matters.
	if err := w.holder.Store(m, served.GetVersion(), w.boundary); err != nil {
		return err
	}

	modelLoads.Inc()
	w.log.Info("model installed",
		"version", served.GetVersion(),
		"trees", m.NumTrees(),
		"nodes", m.NumNodes(),
		"features", m.NumFeatures(),
		"boundary", w.boundary.String(),
		"parse", time.Since(start).String(),
		"metrics", served.GetMetrics())
	return nil
}

// compatible rejects a model whose meaning differs from what this node assumes.
//
// The boundary check is the subtle one. A score from a model trained against a
// ten-minute boundary answers "will this object go unused for ten minutes"; the same
// number from a ten-second model answers a different question and would be ranked as
// if it answered the first. Nothing downstream can detect that, so it is checked here
// against configuration the operator set deliberately.
func (w *Watcher) compatible(meta *beladyv1.ModelMeta) error {
	if meta == nil {
		return errors.New("no metadata")
	}
	if f := meta.GetFormat(); f != "lightgbm-text" {
		return fmt.Errorf("unsupported format %q", f)
	}
	if got := time.Duration(meta.GetBoundarySeconds()) * time.Second; got != w.boundary {
		return fmt.Errorf("trained against a %v Belady boundary, this node is configured for %v", got, w.boundary)
	}
	return nil
}

func (w *Watcher) fetch(ctx context.Context, version string) ([]byte, *beladyv1.ModelMeta, error) {
	stream, err := w.client.GetModel(ctx, &beladyv1.GetModelRequest{Version: version})
	if err != nil {
		return nil, nil, err
	}

	var meta *beladyv1.ModelMeta
	var body []byte
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if m := resp.GetMeta(); m != nil {
			meta = m
			if size := m.GetSizeBytes(); size > 0 {
				body = make([]byte, 0, size)
			}
			continue
		}
		if meta == nil {
			return nil, nil, errors.New("registry sent a chunk before any metadata")
		}
		if uint64(len(body)+len(resp.GetChunk())) > meta.GetSizeBytes() {
			return nil, nil, fmt.Errorf("model body exceeds the declared %d bytes", meta.GetSizeBytes())
		}
		body = append(body, resp.GetChunk()...)
	}
	if meta == nil {
		return nil, nil, errors.New("registry sent no metadata")
	}
	if uint64(len(body)) != meta.GetSizeBytes() {
		return nil, nil, fmt.Errorf("declared %d bytes, received %d", meta.GetSizeBytes(), len(body))
	}
	return body, meta, nil
}
