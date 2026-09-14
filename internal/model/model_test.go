package model

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

// tiny is a hand-written model.txt small enough to verify by hand.
//
// Tree 0:  f0 <= 10 ? (f1 <= 5 ? +0.5 : +0.25) : -0.5
// Tree 1:  f1 <= 2  ? +0.1 : -0.1
// Tree 2:  a stump, always +0.05. LightGBM emits these once boosting stops finding
// a split worth making, and they have no split arrays at all.
const tiny = `tree
version=v3
num_class=1
num_tree_per_iteration=1
label_index=0
max_feature_idx=1
objective=binary sigmoid:1
feature_names=Column_0 Column_1

Tree=0
num_leaves=3
num_cat=0
split_feature=0 1
threshold=10 5
decision_type=2 2
left_child=1 -1
right_child=-2 -3
leaf_value=0.5 -0.5 0.25
is_linear=0
shrinkage=1

Tree=1
num_leaves=2
num_cat=0
split_feature=1
threshold=2
decision_type=2
left_child=-1
right_child=-2
leaf_value=0.1 -0.1
is_linear=0
shrinkage=1

Tree=2
num_leaves=1
num_cat=0
leaf_value=0.05
is_linear=0
shrinkage=1

end of trees

feature_importances:
Column_1=2
Column_0=1
`

func TestTinyModel(t *testing.T) {
	m, err := Load(strings.NewReader(tiny))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.NumFeatures(); got != 2 {
		t.Errorf("NumFeatures = %d, want 2", got)
	}
	if got := m.NumTrees(); got != 3 {
		t.Errorf("NumTrees = %d, want 3", got)
	}
	// Two internal nodes in tree 0, one in tree 1, none in the stump.
	if got := m.NumNodes(); got != 3 {
		t.Errorf("NumNodes = %d, want 3", got)
	}

	cases := []struct {
		features []float32
		want     float32
	}{
		{[]float32{1, 1}, 0.5 + 0.1 + 0.05},
		{[]float32{1, 9}, 0.25 - 0.1 + 0.05},
		{[]float32{20, 1}, -0.5 + 0.1 + 0.05},
		{[]float32{20, 9}, -0.5 - 0.1 + 0.05},
		// Exactly on both thresholds: LightGBM uses <=, so both go left.
		{[]float32{10, 5}, 0.5 - 0.1 + 0.05},
		{[]float32{10, 2}, 0.5 + 0.1 + 0.05},
	}
	for _, c := range cases {
		if got := m.Raw(c.features); math.Abs(float64(got-c.want)) > 1e-6 {
			t.Errorf("Raw(%v) = %v, want %v", c.features, got, c.want)
		}
	}
}

func TestScoreAppliesTheLogistic(t *testing.T) {
	m, err := Load(strings.NewReader(tiny))
	if err != nil {
		t.Fatal(err)
	}
	f := []float32{1, 1}
	raw := float64(m.Raw(f))
	want := 1 / (1 + math.Exp(-raw))
	if got := m.Score(f); math.Abs(got-want) > 1e-9 {
		t.Errorf("Score = %v, want %v", got, want)
	}
	// Monotone, which is the whole reason the eviction path can skip it.
	if m.Score([]float32{20, 9}) >= m.Score([]float32{1, 1}) {
		t.Error("Score is not order-preserving with respect to Raw")
	}
}

// TestRejectsUnsupportedTrees covers the cases where guessing would produce a model
// that loads cleanly and predicts wrongly.
func TestRejectsUnsupportedTrees(t *testing.T) {
	cases := map[string]string{
		"categorical":      strings.Replace(tiny, "num_cat=0", "num_cat=1", 1),
		"missing handling": strings.Replace(tiny, "decision_type=2 2", "decision_type=6 2", 1),
		"linear tree":      strings.Replace(tiny, "is_linear=0", "is_linear=1", 1),
		"multiclass":       strings.Replace(tiny, "num_class=1", "num_class=3", 1),
		"unknown feature":  strings.Replace(tiny, "split_feature=0 1", "split_feature=0 7", 1),
		"array mismatch":   strings.Replace(tiny, "threshold=10 5", "threshold=10", 1),
	}
	for name, text := range cases {
		if _, err := Load(strings.NewReader(text)); err == nil {
			t.Errorf("%s: loaded without error", name)
		}
	}

	if _, err := Load(strings.NewReader("tree\nnum_class=1\nend of trees\n")); err == nil {
		t.Error("a model with no trees loaded without error")
	}
}

// reference is a plain recursive evaluator over LightGBM's own per-tree arrays. It
// exists to check the flattening: rebasing every child index and leaf index onto
// shared arrays is where a subtle off-by-one would hide, and it would show up as a
// slightly worse hit ratio rather than a crash.
type reference struct {
	trees []refTree
}

type refTree struct {
	splitFeature []int32
	threshold    []float32
	left, right  []int32
	leafValue    []float32
}

func (r reference) raw(f []float32) float32 {
	var sum float32
	for _, t := range r.trees {
		i := int32(0)
		for {
			if f[t.splitFeature[i]] <= t.threshold[i] {
				i = t.left[i]
			} else {
				i = t.right[i]
			}
			if i < 0 {
				sum += t.leafValue[^i]
				break
			}
		}
	}
	return sum
}

// synth builds a complete binary tree of the given depth per tree, in the level
// order LightGBM uses, and returns both the model text and a reference evaluator.
func synth(trees, depth, features int, rng *rand.Rand) (string, reference) {
	internal := 1<<depth - 1
	firstDeepest := 1<<(depth-1) - 1

	var b strings.Builder
	fmt.Fprintf(&b, "tree\nnum_class=1\nnum_tree_per_iteration=1\nmax_feature_idx=%d\nobjective=binary sigmoid:1\n\n", features-1)

	var ref reference
	for tr := range trees {
		t := refTree{
			splitFeature: make([]int32, internal),
			threshold:    make([]float32, internal),
			left:         make([]int32, internal),
			right:        make([]int32, internal),
			leafValue:    make([]float32, 1<<depth),
		}
		for i := range internal {
			t.splitFeature[i] = int32(rng.IntN(features))
			t.threshold[i] = float32(rng.Float64() * 100)
			l, r := 2*i+1, 2*i+2
			if l < internal {
				t.left[i], t.right[i] = int32(l), int32(r)
			} else {
				// Deepest internal level: children are leaves, numbered left to right.
				leaf := int32(2 * (i - firstDeepest))
				t.left[i], t.right[i] = ^leaf, ^(leaf + 1)
			}
		}
		for i := range t.leafValue {
			t.leafValue[i] = float32(rng.Float64()*2 - 1)
		}
		ref.trees = append(ref.trees, t)

		fmt.Fprintf(&b, "Tree=%d\nnum_leaves=%d\nnum_cat=0\n", tr, 1<<depth)
		fmt.Fprintf(&b, "split_feature=%s\n", join(t.splitFeature))
		fmt.Fprintf(&b, "threshold=%s\n", joinF(t.threshold))
		fmt.Fprintf(&b, "decision_type=%s\n", strings.TrimSpace(strings.Repeat("2 ", internal)))
		fmt.Fprintf(&b, "left_child=%s\n", join(t.left))
		fmt.Fprintf(&b, "right_child=%s\n", join(t.right))
		fmt.Fprintf(&b, "leaf_value=%s\nis_linear=0\nshrinkage=1\n\n", joinF(t.leafValue))
	}
	b.WriteString("end of trees\n")
	return b.String(), ref
}

func join(v []int32) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, " ")
}

func joinF(v []float32) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprintf("%.17g", x)
	}
	return strings.Join(parts, " ")
}

func TestFlatEvaluatorMatchesAReferenceWalk(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	const features = 12
	text, ref := synth(40, 5, features, rng)

	m, err := Load(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if m.NumTrees() != 40 {
		t.Fatalf("NumTrees = %d, want 40", m.NumTrees())
	}

	f := make([]float32, features)
	for range 20_000 {
		for i := range f {
			f[i] = float32(rng.Float64() * 100)
		}
		got, want := m.Raw(f), ref.raw(f)
		// Summation order is identical, so this should be bit-exact.
		if got != want {
			t.Fatalf("Raw = %v, reference = %v, features = %v", got, want, f)
		}
	}
}

// BenchmarkRaw is the budget check. One eviction evaluates the model once per
// sampled candidate, so with a sample of 8 the per-eviction model cost is eight
// times whatever this reports, and it has to stay well inside a microsecond.
func BenchmarkRaw(b *testing.B) {
	for _, cfg := range []struct{ trees, depth int }{{50, 5}, {100, 6}, {200, 6}} {
		rng := rand.New(rand.NewPCG(9, 1))
		const features = 16
		text, _ := synth(cfg.trees, cfg.depth, features, rng)
		m, err := Load(strings.NewReader(text))
		if err != nil {
			b.Fatal(err)
		}
		f := make([]float32, features)
		for i := range f {
			f[i] = float32(rng.Float64() * 100)
		}

		b.Run(fmt.Sprintf("trees=%d/leaves=%d", cfg.trees, 1<<cfg.depth), func(b *testing.B) {
			b.ReportAllocs()
			var sink float32
			for b.Loop() {
				sink = m.Raw(f)
			}
			// A sample of 8 candidates per eviction is the configured default.
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)*8, "ns/eviction")
			if sink == 0 && m.NumTrees() == 0 {
				b.Fatal("unreachable, keeps sink live")
			}
		})
	}
}

func BenchmarkLoad(b *testing.B) {
	text, _ := synth(100, 6, 16, rand.New(rand.NewPCG(1, 2)))
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Load(strings.NewReader(text)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRawVaryingFeatures is BenchmarkRaw with a different feature vector on
// every evaluation, which is the only thing the live cache ever does: eight
// candidates per eviction, each with its own recency, size and access history.
//
// It exists because the live eviction cost came in far above what BenchmarkRaw
// predicted, and BenchmarkRaw evaluates one hot vector in a loop. A tree walk is a
// chain of data-dependent branches, so a fixed vector takes the same path every
// time and the branch predictor learns all of it. Nothing live is predictable that
// way, and this measures the difference.
//
// The vectors are drawn from the same uniform range as the synthetic thresholds, so
// each comparison is close to a coin flip. That is the worst case rather than the
// average one, and the point is the size of the effect, not a number to quote as
// the model's cost.
func BenchmarkRawVaryingFeatures(b *testing.B) {
	const (
		features = 16
		vectors  = 512
	)
	rng := rand.New(rand.NewPCG(9, 1))
	text, _ := synth(13, 5, features, rng)
	m, err := Load(strings.NewReader(text))
	if err != nil {
		b.Fatal(err)
	}

	// One flat backing array, so walking the set costs a sequential read and does
	// not import a cache-miss effect into a branch measurement.
	flat := make([]float32, vectors*features)
	for i := range flat {
		flat[i] = float32(rng.Float64() * 100)
	}

	b.Run("hot", func(b *testing.B) {
		b.ReportAllocs()
		f := flat[:features]
		var sink float32
		for b.Loop() {
			sink = m.Raw(f)
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)*8, "ns/eviction")
		keepAlive(b, m, sink)
	})

	b.Run("varying", func(b *testing.B) {
		b.ReportAllocs()
		var sink float32
		i := 0
		for b.Loop() {
			sink = m.Raw(flat[i : i+features])
			if i += features; i == len(flat) {
				i = 0
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)*8, "ns/eviction")
		keepAlive(b, m, sink)
	})
}

func keepAlive(b *testing.B, m *Model, sink float32) {
	b.Helper()
	if sink == 0 && m.NumTrees() == 0 {
		b.Fatal("unreachable, keeps sink live")
	}
}
