package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/features"
)

// dial starts the real service on a real socket. The streaming semantics are the
// part most likely to be wrong, and they only exist over a connection.
func dial(t *testing.T, dir string) beladyv1.RegistryClient {
	t.Helper()
	srv, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	beladyv1.RegisterRegistryServer(g, srv)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return beladyv1.NewRegistryClient(conn)
}

func metaFor(version string, body []byte) *beladyv1.ModelMeta {
	sum := sha256.Sum256(body)
	return &beladyv1.ModelMeta{
		Version:         version,
		Format:          "lightgbm-text",
		SizeBytes:       uint64(len(body)),
		Sha256:          hex.EncodeToString(sum[:]),
		FeatureCount:    features.Count,
		BoundarySeconds: 600,
	}
}

func publish(t *testing.T, c beladyv1.RegistryClient, meta *beladyv1.ModelMeta, body []byte) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := c.PublishModel(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&beladyv1.PublishModelRequest{
		Payload: &beladyv1.PublishModelRequest_Meta{Meta: meta},
	}); err != nil {
		return err
	}
	// Chunk small on purpose, so reassembly is actually exercised.
	for off := 0; off < len(body); off += 7 {
		end := min(off+7, len(body))
		if err := stream.Send(&beladyv1.PublishModelRequest{
			Payload: &beladyv1.PublishModelRequest_Chunk{Chunk: body[off:end]},
		}); err != nil {
			return err
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

func get(t *testing.T, c beladyv1.RegistryClient, version string) (*beladyv1.ModelMeta, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := c.GetModel(ctx, &beladyv1.GetModelRequest{Version: version})
	if err != nil {
		return nil, nil, err
	}
	var meta *beladyv1.ModelMeta
	var body []byte
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return meta, body, nil
		}
		if err != nil {
			return meta, body, err
		}
		if m := resp.GetMeta(); m != nil {
			meta = m
		}
		body = append(body, resp.GetChunk()...)
	}
}

func TestPublishGetAndWatch(t *testing.T) {
	dir := t.TempDir()
	c := dial(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Watch before anything exists, which is what a cache node starting on a fresh
	// cluster does.
	watch, err := c.WatchModels(ctx, &beladyv1.WatchModelsRequest{})
	if err != nil {
		t.Fatal(err)
	}

	body := []byte("tree=0\nnum_leaves=2\n")
	if err := publish(t, c, metaFor("20260909T120000Z", body), body); err != nil {
		t.Fatalf("publish: %v", err)
	}

	event, err := watch.Recv()
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if got := event.GetMeta().GetVersion(); got != "20260909T120000Z" {
		t.Fatalf("watch reported version %q", got)
	}
	if event.GetMeta().GetCreatedUnix() == 0 {
		t.Error("created_unix was not stamped, so publication order is unrecoverable")
	}

	// An empty version means "the newest", which is how a node bootstraps.
	meta, got, err := get(t, c, "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("round-tripped %q, want %q", got, body)
	}
	if meta.GetBoundarySeconds() != 600 || meta.GetFeatureCount() != features.Count {
		t.Errorf("metadata lost in transit: %+v", meta)
	}

	// A second publish wakes the same stream again without a reconnect.
	body2 := []byte("tree=0\nnum_leaves=4\n")
	if err := publish(t, c, metaFor("20260909T130000Z", body2), body2); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	event, err = watch.Recv()
	if err != nil {
		t.Fatalf("second watch: %v", err)
	}
	if got := event.GetMeta().GetVersion(); got != "20260909T130000Z" {
		t.Fatalf("second event reported version %q", got)
	}

	list, err := c.ListModels(ctx, &beladyv1.ListModelsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetModels()) != 2 || list.GetModels()[0].GetVersion() != "20260909T130000Z" {
		t.Errorf("list is not newest-first: %v", list.GetModels())
	}
}

// TestWatchCatchesUpImmediately is the property that makes a restart safe. A node
// that comes back with no model must be handed the current one, not left running its
// fallback policy until the next training run hours later.
func TestWatchCatchesUpImmediately(t *testing.T) {
	dir := t.TempDir()
	c := dial(t, dir)

	body := []byte("tree=0\n")
	if err := publish(t, c, metaFor("20260101T000000Z", body), body); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watch, err := c.WatchModels(ctx, &beladyv1.WatchModelsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	event, err := watch.Recv()
	if err != nil {
		t.Fatalf("a watcher with no version was not caught up: %v", err)
	}
	if event.GetMeta().GetVersion() != "20260101T000000Z" {
		t.Errorf("caught up to %q", event.GetMeta().GetVersion())
	}

	// A watcher already on the current version gets nothing, rather than a
	// redundant event that would make the node reload the model it already has.
	current, err := c.WatchModels(ctx, &beladyv1.WatchModelsRequest{SinceVersion: "20260101T000000Z"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := current.Recv(); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("an up-to-date watcher received an event (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRestartAdoptsTheNewestModel(t *testing.T) {
	dir := t.TempDir()
	c := dial(t, dir)
	for _, v := range []string{"20260101T000000Z", "20260301T000000Z", "20260201T000000Z"} {
		body := []byte("tree=" + v)
		if err := publish(t, c, metaFor(v, body), body); err != nil {
			t.Fatal(err)
		}
	}

	// A fresh store over the same directory is what a restart looks like.
	st, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	latest, _ := st.snapshot()
	if latest.GetVersion() != "20260301T000000Z" {
		t.Errorf("restart adopted %q, want the newest published version", latest.GetVersion())
	}
}

func TestPublishRejectsBadMetadata(t *testing.T) {
	body := []byte("tree=0\n")
	cases := []struct {
		name   string
		mutate func(*beladyv1.ModelMeta)
	}{
		{"empty version", func(m *beladyv1.ModelMeta) { m.Version = "" }},
		{"path traversal", func(m *beladyv1.ModelMeta) { m.Version = "../../etc/passwd" }},
		{"dotted version", func(m *beladyv1.ModelMeta) { m.Version = "v1.0" }},
		{"wrong format", func(m *beladyv1.ModelMeta) { m.Format = "onnx" }},
		{"no feature count", func(m *beladyv1.ModelMeta) { m.FeatureCount = 0 }},
		{"no boundary", func(m *beladyv1.ModelMeta) { m.BoundarySeconds = 0 }},
		{"wrong size", func(m *beladyv1.ModelMeta) { m.SizeBytes = uint64(len(body)) + 1 }},
		{"wrong digest", func(m *beladyv1.ModelMeta) { m.Sha256 = "00" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := dial(t, dir)
			meta := metaFor("20260909T120000Z", body)
			tc.mutate(meta)

			if err := publish(t, c, meta, body); err == nil {
				t.Fatal("publish accepted it")
			}
			// Nothing may be left behind, or a later GetModel would serve a model the
			// registry already decided it could not vouch for.
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if filepath.Ext(e.Name()) == modelExt {
					t.Errorf("rejected publish left %s on disk", e.Name())
				}
			}
		})
	}
}

// TestGetModelRejectsTraversal covers the read path. Publishing validates the
// version because it becomes a filename, but GetModel took it straight from the
// request, so a version of "../secret" escaped MODEL_DIR entirely.
func TestGetModelRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "models")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A model the operator never published, one directory up from MODEL_DIR.
	body := []byte("tree\nnot a model the registry owns\n")
	planted := metaFor("secret", body)
	raw, err := proto.Marshal(planted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret"+metaExt), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret"+modelExt), body, 0o600); err != nil {
		t.Fatal(err)
	}

	c := dial(t, dir)
	for _, version := range []string{"../secret", "..%2Fsecret", "../../etc/passwd", "sub/secret", `..\secret`} {
		if _, got, err := get(t, c, version); err == nil {
			t.Errorf("GetModel(%q) succeeded and returned %d bytes; want a rejection", version, len(got))
		}
	}
}

func TestGetUnknownVersionIsNotFound(t *testing.T) {
	c := dial(t, t.TempDir())
	if _, _, err := get(t, c, "20260909T120000Z"); err == nil {
		t.Error("getting a model that was never published succeeded")
	}
	if _, _, err := get(t, c, ""); err == nil {
		t.Error("getting the latest model from an empty registry succeeded")
	}
}
