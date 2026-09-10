// Package registry is the handoff point between the Python trainer and the Go cache
// nodes: the trainer publishes a model, cache nodes watch for it and pull it.
//
// It is deliberately a directory of files with a gRPC face on it. A model is a few
// hundred kilobytes of text published a few times a day, so the interesting
// properties are that a half-written model can never be served, that a cache node
// restarting picks up the current model without waiting for the next training run,
// and that a bad model can be identified by version afterwards. None of those want a
// database.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
)

const (
	modelExt = ".model"
	metaExt  = ".meta"
	tmpExt   = ".partial"

	// chunkBytes is the streaming unit. Well under grpcx.MaxRecvBytes, because the
	// point of chunking is that model size is unbounded in principle even though it
	// is small in practice.
	chunkBytes = 256 << 10
)

// store is a versioned directory of models.
//
// Watchers are not tracked individually. Instead every publish replaces latest and
// closes changed, and a watcher waits on the channel it read alongside latest. That
// gives every watcher exactly one wakeup per publish with no subscriber registry to
// leak, and a watcher that goes away leaks nothing at all.
type store struct {
	latest  *beladyv1.ModelMeta
	changed chan struct{}
	dir     string

	// mu guards latest and changed. dir is set once at construction.
	mu sync.Mutex
}

func newStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	s := &store{dir: dir, changed: make(chan struct{})}

	versions, err := s.versions()
	if err != nil {
		return nil, err
	}
	if len(versions) > 0 {
		// Adopt the newest model on the disk, so restarting the registry does not
		// silently roll the cluster back to no model at all.
		meta, err := s.meta(versions[len(versions)-1])
		if err != nil {
			return nil, err
		}
		s.latest = meta
	}
	return s, nil
}

// versions lists the models on disk, oldest first. Versions are timestamps in a
// sortable form, so lexical order is publication order.
func (s *store) versions() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), modelExt); ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *store) paths(version string) (model, meta string) {
	base := filepath.Join(s.dir, version)
	return base + modelExt, base + metaExt
}

func (s *store) meta(version string) (*beladyv1.ModelMeta, error) {
	_, metaPath := s.paths(version)
	raw, err := os.ReadFile(metaPath) //nolint:gosec // version is validated before it reaches here
	if err != nil {
		return nil, err
	}
	var meta beladyv1.ModelMeta
	if err := proto.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("model %s has unreadable metadata: %w", version, err)
	}
	return &meta, nil
}

func (s *store) snapshot() (*beladyv1.ModelMeta, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest, s.changed
}

// publish writes the model and its metadata, then makes it visible.
//
// The model file is renamed into place before the metadata is written, and the
// metadata is what readers look for, so a reader can never find a version whose
// bytes are still arriving.
func (s *store) publish(meta *beladyv1.ModelMeta, body []byte) error {
	modelPath, metaPath := s.paths(meta.GetVersion())
	if err := writeAtomic(modelPath, body); err != nil {
		return err
	}
	encoded, err := proto.Marshal(meta)
	if err != nil {
		return err
	}
	if err := writeAtomic(metaPath, encoded); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Guard against an out-of-order publish overwriting a newer model as latest. Two
	// trainer runs finishing close together is unlikely but the cost of being wrong
	// is a silent rollback.
	if s.latest == nil || meta.GetVersion() > s.latest.GetVersion() {
		s.latest = meta
		close(s.changed)
		s.changed = make(chan struct{})
	}
	return nil
}

func writeAtomic(path string, body []byte) error {
	if err := os.WriteFile(path+tmpExt, body, 0o600); err != nil {
		return err
	}
	return os.Rename(path+tmpExt, path)
}

type Server struct {
	beladyv1.UnimplementedRegistryServer
	store *store
}

func (s *Server) PublishModel(stream beladyv1.Registry_PublishModelServer) error {
	var meta *beladyv1.ModelMeta
	var body []byte

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch payload := req.GetPayload().(type) {
		case *beladyv1.PublishModelRequest_Meta:
			if meta != nil {
				return status.Error(codes.InvalidArgument, "metadata sent twice")
			}
			meta = payload.Meta
		case *beladyv1.PublishModelRequest_Chunk:
			if meta == nil {
				return status.Error(codes.InvalidArgument, "first message must carry metadata")
			}
			if len(body)+len(payload.Chunk) > int(meta.GetSizeBytes()) {
				// Bound the buffer by the declared size rather than trusting the stream to
				// end: an unbounded append here is a memory exhaustion primitive.
				return status.Errorf(codes.InvalidArgument, "model body exceeds the declared %d bytes",
					meta.GetSizeBytes())
			}
			body = append(body, payload.Chunk...)
		default:
			return status.Error(codes.InvalidArgument, "empty payload")
		}
	}

	if meta == nil {
		return status.Error(codes.InvalidArgument, "no metadata")
	}
	if err := validate(meta, body); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if meta.GetCreatedUnix() == 0 {
		meta.CreatedUnix = time.Now().Unix()
	}
	if err := s.store.publish(meta, body); err != nil {
		return status.Errorf(codes.Internal, "publish failed: %v", err)
	}

	slog.Info("model published", "version", meta.GetVersion(), "bytes", len(body),
		"features", meta.GetFeatureCount(), "boundary_seconds", meta.GetBoundarySeconds())
	return stream.SendAndClose(&beladyv1.PublishModelResponse{Meta: meta})
}

// validVersion rejects anything that is not a bare filename. A version becomes a
// path component in MODEL_DIR, so this has to hold on every path that turns one
// into a filename: publishing, where it arrives from the trainer, and GetModel,
// where it arrives straight from an untrusted request.
func validVersion(version string) error {
	switch {
	case version == "":
		return errors.New("version is required")
	case len(version) > 64:
		return errors.New("version is too long")
	case version != filepath.Base(version), strings.ContainsAny(version, `/\.`):
		return fmt.Errorf("version %q is not a bare name", version)
	}
	return nil
}

// validate is the trust boundary. The trainer is a separate process in another
// language, so everything it asserts about the blob is checked here rather than
// assumed, and version is checked as a path component because it becomes a filename.
func validate(meta *beladyv1.ModelMeta, body []byte) error {
	if err := validVersion(meta.GetVersion()); err != nil {
		return err
	}
	if f := meta.GetFormat(); f != "lightgbm-text" {
		return fmt.Errorf("unsupported format %q", f)
	}
	if meta.GetFeatureCount() == 0 {
		return errors.New("feature_count is required: a cache node cannot check compatibility without it")
	}
	if meta.GetBoundarySeconds() == 0 {
		return errors.New("boundary_seconds is required: the prediction is meaningless without the boundary it was trained against")
	}
	if len(body) == 0 {
		return errors.New("model body is empty")
	}
	if uint64(len(body)) != meta.GetSizeBytes() {
		return fmt.Errorf("declared %d bytes, received %d", meta.GetSizeBytes(), len(body))
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != meta.GetSha256() {
		return fmt.Errorf("sha256 mismatch: declared %s, computed %s", meta.GetSha256(), got)
	}
	return nil
}

func (s *Server) GetModel(req *beladyv1.GetModelRequest, stream beladyv1.Registry_GetModelServer) error {
	version := req.GetVersion()
	if version == "" {
		meta, _ := s.store.snapshot()
		if meta == nil {
			return status.Error(codes.NotFound, "no model has been published")
		}
		version = meta.GetVersion()
	} else if err := validVersion(version); err != nil {
		// An empty version resolved from the snapshot above, so it is already a
		// name this registry wrote. Anything else is caller-supplied.
		return status.Errorf(codes.InvalidArgument, "%v", err)
	}

	meta, err := s.store.meta(version)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status.Errorf(codes.NotFound, "no model %s", version)
		}
		return status.Errorf(codes.Internal, "reading model %s: %v", version, err)
	}
	if err := stream.Send(&beladyv1.GetModelResponse{
		Payload: &beladyv1.GetModelResponse_Meta{Meta: meta},
	}); err != nil {
		return err
	}

	modelPath, _ := s.store.paths(version)
	f, err := os.Open(modelPath) //nolint:gosec // version passed validVersion above
	if err != nil {
		return status.Errorf(codes.Internal, "opening model %s: %v", version, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, chunkBytes)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&beladyv1.GetModelResponse{
				Payload: &beladyv1.GetModelResponse_Chunk{Chunk: buf[:n]},
			}); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "reading model %s: %v", version, err)
		}
	}
}

// WatchModels sends an event for every model newer than the watcher's version,
// starting with one immediately if the registry is already ahead. A cache node that
// restarts therefore converges without waiting for the next training run, which is
// the whole reason this is a watch rather than a poll.
func (s *Server) WatchModels(req *beladyv1.WatchModelsRequest, stream beladyv1.Registry_WatchModelsServer) error {
	seen := req.GetSinceVersion()
	for {
		meta, changed := s.store.snapshot()
		if meta != nil && meta.GetVersion() > seen {
			if err := stream.Send(&beladyv1.ModelEvent{Meta: meta}); err != nil {
				return err
			}
			seen = meta.GetVersion()
			continue
		}
		select {
		case <-changed:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

func (s *Server) ListModels(_ context.Context, req *beladyv1.ListModelsRequest) (*beladyv1.ListModelsResponse, error) {
	versions, err := s.store.versions()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing models: %v", err)
	}
	slices.Reverse(versions) // newest first, as the proto promises

	limit := int(req.GetLimit())
	if limit <= 0 || limit > len(versions) {
		limit = len(versions)
	}
	out := make([]*beladyv1.ModelMeta, 0, limit)
	for _, v := range versions[:limit] {
		meta, err := s.store.meta(v)
		if err != nil {
			// A model whose metadata is unreadable is not a reason to fail the listing;
			// skip it and say so, because the operator needs the rest of the list to
			// diagnose it.
			slog.Warn("skipping model with unreadable metadata", "version", v, "err", err)
			continue
		}
		out = append(out, meta)
	}
	return &beladyv1.ListModelsResponse{Models: out}, nil
}

// New opens or creates the model directory and returns a service ready to register.
func New(dir string) (*Server, error) {
	st, err := newStore(dir)
	if err != nil {
		return nil, err
	}
	return &Server{store: st}, nil
}

// Latest reports the version the registry would serve for an empty request, or ""
// when nothing has been published. Logged at startup so an operator can tell a fresh
// registry from one that lost its volume.
func (s *Server) Latest() string {
	meta, _ := s.store.snapshot()
	return meta.GetVersion()
}
