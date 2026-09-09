package registry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	beladyv1 "github.com/JustinK33/newproj/gen/belady/v1"
	"github.com/JustinK33/newproj/internal/cache"
	"github.com/JustinK33/newproj/internal/features"
)

// lightgbmModel writes a minimal but genuine LightGBM text dump: one tree that
// thresholds on the recency column. The evaluator's own tests cover parsing; what
// matters here is that a real file survives the round trip through publish, watch,
// fetch and install.
func lightgbmModel(trees int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tree\nversion=v3\nnum_class=1\nnum_tree_per_iteration=1\nlabel_index=0\nmax_feature_idx=%d\nobjective=binary sigmoid:1\n\n",
		features.Count-1)
	b.WriteString("Tree=0\nnum_leaves=2\nnum_cat=0\n")
	fmt.Fprintf(&b, "split_feature=%d\n", features.RecencyMS)
	b.WriteString("threshold=1000\n")
	b.WriteString("decision_type=2\n")
	b.WriteString("left_child=-1\n")
	b.WriteString("right_child=-2\n")
	b.WriteString("leaf_value=0.25 -0.25\n\n")
	// Constant stumps pad the file out, so the body spans several stream chunks
	// rather than fitting in one message.
	for i := 1; i < trees; i++ {
		fmt.Fprintf(&b, "Tree=%d\nnum_leaves=1\nnum_cat=0\nsplit_feature=\nthreshold=\ndecision_type=\nleft_child=\nright_child=\nleaf_value=0.01\n\n", i)
	}
	b.WriteString("end of trees\n")
	return b.String()
}

func runWatcher(t *testing.T, c beladyv1.RegistryClient, holder *cache.ModelHolder, boundary time.Duration) {
	t.Helper()
	w := &Watcher{
		client:   c,
		holder:   holder,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		boundary: boundary,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
}

// eventually polls for a condition. Model installation crosses two streams and a
// parse, so the test cannot assert on it synchronously without reaching inside the
// watcher and testing something other than what runs in production.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatcherInstallsAPublishedModel(t *testing.T) {
	c := dial(t, t.TempDir())
	holder := cache.NewModelHolder()
	runWatcher(t, c, holder, 10*time.Minute)

	body := []byte(lightgbmModel(40))
	meta := metaFor("20260909T120000Z", body)
	meta.BoundarySeconds = 600
	meta.Metrics = map[string]string{"auc": "0.81"}
	if err := publish(t, c, meta, body); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the model to be installed", func() bool {
		return holder.Version() == "20260909T120000Z"
	})
	if got := holder.Boundary(); got != 10*time.Minute {
		t.Errorf("installed with boundary %v", got)
	}

	// A second publish rolls forward without a restart, which is the point of the
	// watch stream.
	body2 := []byte(lightgbmModel(60))
	meta2 := metaFor("20260909T130000Z", body2)
	meta2.BoundarySeconds = 600
	if err := publish(t, c, meta2, body2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second model to replace the first", func() bool {
		return holder.Version() == "20260909T130000Z"
	})
}

// TestWatcherRefusesAMismatchedBoundary is the check that keeps a hit-ratio number
// honest. A model trained against a different boundary answers a different question,
// and its scores would be ranked as if it answered this node's.
func TestWatcherRefusesAMismatchedBoundary(t *testing.T) {
	c := dial(t, t.TempDir())
	holder := cache.NewModelHolder()
	runWatcher(t, c, holder, 10*time.Minute)

	body := []byte(lightgbmModel(2))
	meta := metaFor("20260909T120000Z", body)
	meta.BoundarySeconds = 30
	if err := publish(t, c, meta, body); err != nil {
		t.Fatal(err)
	}

	// Give the watcher room to get it wrong.
	time.Sleep(300 * time.Millisecond)
	if v := holder.Version(); v != "" {
		t.Fatalf("installed %q despite a 30s boundary against a 10m node", v)
	}

	// And it must keep watching: the next run may be correct, and a node that tore
	// down the stream on a bad model would stay on its fallback until a restart.
	good := []byte(lightgbmModel(3))
	goodMeta := metaFor("20260909T130000Z", good)
	goodMeta.BoundarySeconds = 600
	if err := publish(t, c, goodMeta, good); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the watcher to recover and install a valid model", func() bool {
		return holder.Version() == "20260909T130000Z"
	})
}

func TestWatcherRefusesAnUnparseableModel(t *testing.T) {
	c := dial(t, t.TempDir())
	holder := cache.NewModelHolder()
	runWatcher(t, c, holder, 10*time.Minute)

	body := []byte("this is not a lightgbm dump\n")
	meta := metaFor("20260909T120000Z", body)
	meta.BoundarySeconds = 600
	if err := publish(t, c, meta, body); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)
	if v := holder.Version(); v != "" {
		t.Fatalf("installed unparseable bytes as %q", v)
	}
}

// TestWatcherCatchesUpOnStart covers the restart path: the model exists before the
// node does, and nothing will be published for hours.
func TestWatcherCatchesUpOnStart(t *testing.T) {
	c := dial(t, t.TempDir())

	body := []byte(lightgbmModel(5))
	meta := metaFor("20260101T000000Z", body)
	meta.BoundarySeconds = 600
	if err := publish(t, c, meta, body); err != nil {
		t.Fatal(err)
	}

	holder := cache.NewModelHolder()
	runWatcher(t, c, holder, 10*time.Minute)
	eventually(t, "a starting node to catch up to the current model", func() bool {
		return holder.Version() == "20260101T000000Z"
	})
}
