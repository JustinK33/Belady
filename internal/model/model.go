// Package model loads a LightGBM text model and evaluates it fast enough to run
// inside the eviction path.
//
// Nothing here calls into C. LightGBM's own predictor is excellent but reaching it
// from Go means CGO, which costs a stack switch per call and makes the build
// non-static; ONNX Runtime means a much larger dependency for one gradient-boosted
// tree ensemble. A GBDT predictor is a threshold comparison and two array indices
// per level, so it is worth hand-rolling. See docs/adr/0004.
//
// The layout is the reason it is fast. Every node of every tree lives in one
// contiguous slice of 16-byte structs, so a walk down a tree touches sequential-ish
// memory with no pointer chasing, no per-tree bounds setup, and no interface
// dispatch in the inner loop.
package model

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// node is 16 bytes, so four fit in a cache line.
//
// left and right encode both destinations in one int32: a non-negative value is an
// index into nodes, a negative value is the leaf ^v. That removes the "is this a
// leaf" flag and its branch from the walk.
type node struct {
	threshold float32
	feature   int32
	left      int32
	right     int32
}

// Model is immutable once loaded, so a pointer to one can be swapped in atomically
// while requests are reading the old one.
type Model struct {
	objective string

	nodes  []node
	leaves []float32
	// roots[i] is where tree i starts, using the same encoding as node.left, so a
	// single-leaf tree (LightGBM emits these when a split gains nothing) needs no
	// special case in Raw.
	roots []int32

	sigmoid     float64
	numFeatures int
}

// Load parses a LightGBM model.txt.
func Load(r io.Reader) (*Model, error) {
	p := parser{sc: bufio.NewScanner(r), m: &Model{sigmoid: 1}}
	p.sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	if err := p.run(); err != nil {
		return nil, err
	}
	if len(p.m.roots) == 0 {
		return nil, fmt.Errorf("model: no trees found")
	}
	if p.m.numFeatures == 0 {
		return nil, fmt.Errorf("model: max_feature_idx missing")
	}
	return p.m, nil
}

func (m *Model) NumFeatures() int { return m.numFeatures }
func (m *Model) NumTrees() int    { return len(m.roots) }
func (m *Model) NumNodes() int    { return len(m.nodes) }
func (m *Model) Objective() string {
	return m.objective
}

// Raw returns the summed leaf values across every tree.
//
// This is the hot path: it runs once per eviction candidate, so a sample of 8 means
// eight calls per eviction. It allocates nothing and has no bounds-check-defeating
// tricks; the bounds checks that remain are the price of not using unsafe.
//
// features must have at least NumFeatures entries and must not contain NaN. Both
// are guaranteed by internal/features and checked once at model-swap time rather
// than per candidate, because a length check here would cost more than the walk.
func (m *Model) Raw(features []float32) float32 {
	var sum float32
	for _, i := range m.roots {
		for i >= 0 {
			n := &m.nodes[i]
			if features[n.feature] <= n.threshold {
				i = n.left
			} else {
				i = n.right
			}
		}
		sum += m.leaves[^i]
	}
	return sum
}

// Score is Raw put through the objective's link function, so the result is the
// probability the trainer was fitting.
//
// The eviction path deliberately does not call this. Ranking candidates only needs
// order, the logistic is monotone, and skipping it saves an Exp per candidate.
func (m *Model) Score(features []float32) float64 {
	raw := float64(m.Raw(features))
	if strings.HasPrefix(m.objective, "binary") {
		return 1 / (1 + math.Exp(-m.sigmoid*raw))
	}
	return raw
}

type parser struct {
	sc *bufio.Scanner
	m  *Model

	// Accumulated per-tree arrays, reused across trees.
	splitFeature []int32
	threshold    []float32
	decisionType []int64
	leftChild    []int32
	rightChild   []int32
	leafValue    []float32
	numLeaves    int
	inTree       bool
	line         int
}

func (p *parser) run() error {
	for p.sc.Scan() {
		p.line++
		line := strings.TrimSpace(p.sc.Text())

		switch line {
		case "":
			continue
		case "end of trees":
			return p.flush()
		case "tree":
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// Trailing sections (feature importances, parameters) are free-form text
			// with no bearing on prediction.
			continue
		}

		var err error
		switch key {
		case "Tree":
			err = p.flush()
			p.inTree = true
		case "objective":
			p.m.objective = value
			p.parseSigmoid(value)
		case "num_class", "num_tree_per_iteration":
			err = p.expectOne(key, value)
		case "max_feature_idx":
			var n int
			if n, err = strconv.Atoi(value); err == nil {
				p.m.numFeatures = n + 1
			}
		case "num_leaves":
			p.numLeaves, err = strconv.Atoi(value)
		case "num_cat":
			if value != "0" {
				err = fmt.Errorf("categorical splits are not supported; train with only numerical features")
			}
		case "is_linear":
			if value != "0" {
				err = fmt.Errorf("linear trees are not supported; train with linear_tree=false")
			}
		case "split_feature":
			p.splitFeature, err = ints32(value)
		case "threshold":
			p.threshold, err = floats32(value)
		case "decision_type":
			p.decisionType, err = ints64(value)
		case "left_child":
			p.leftChild, err = ints32(value)
		case "right_child":
			p.rightChild, err = ints32(value)
		case "leaf_value":
			p.leafValue, err = floats32(value)
		}
		if err != nil {
			return fmt.Errorf("model: line %d (%s): %w", p.line, key, err)
		}
	}
	if err := p.sc.Err(); err != nil {
		return fmt.Errorf("model: %w", err)
	}
	return p.flush()
}

// flush converts the arrays of the tree just read into flat nodes.
func (p *parser) flush() error {
	if !p.inTree {
		return nil
	}
	p.inTree = false

	leafBase := int32(len(p.m.leaves))
	p.m.leaves = append(p.m.leaves, p.leafValue...)

	// A tree that never split: the root is the single leaf.
	if p.numLeaves <= 1 {
		if len(p.leafValue) != 1 {
			return fmt.Errorf("model: %d leaves declared but %d leaf values", p.numLeaves, len(p.leafValue))
		}
		p.m.roots = append(p.m.roots, ^leafBase)
		p.reset()
		return nil
	}

	internal := p.numLeaves - 1
	if len(p.splitFeature) != internal || len(p.threshold) != internal ||
		len(p.leftChild) != internal || len(p.rightChild) != internal ||
		len(p.decisionType) != internal || len(p.leafValue) != p.numLeaves {
		return fmt.Errorf("model: tree arrays disagree with num_leaves=%d", p.numLeaves)
	}

	base := int32(len(p.m.nodes))
	for i := range internal {
		if err := p.checkDecisionType(p.decisionType[i]); err != nil {
			return err
		}
		if f := p.splitFeature[i]; f < 0 || int(f) >= p.m.numFeatures {
			return fmt.Errorf("model: split on feature %d, model declares %d", f, p.m.numFeatures)
		}
		p.m.nodes = append(p.m.nodes, node{
			feature:   p.splitFeature[i],
			threshold: p.threshold[i],
			left:      p.child(p.leftChild[i], base, leafBase),
			right:     p.child(p.rightChild[i], base, leafBase),
		})
	}
	p.m.roots = append(p.m.roots, base)
	p.reset()
	return nil
}

// child rebases one of LightGBM's per-tree child indices onto the flat arrays.
// LightGBM writes a non-negative value for an internal node and ^leaf for a leaf;
// the same convention is kept, so only the offsets change.
func (p *parser) child(v, nodeBase, leafBase int32) int32 {
	if v >= 0 {
		return nodeBase + v
	}
	return ^(leafBase + ^v)
}

// checkDecisionType refuses anything the two-branch walk in Raw cannot express.
//
// LightGBM's own predictor routes a "missing" value to a per-node default side
// before comparing against the threshold, and by default it treats zero as missing.
// Half the features here are legitimately zero (an object seen once has no
// inter-arrival deltas), so silently ignoring that flag would mispredict on exactly
// the candidates that matter. The trainer sets use_missing=false and
// zero_as_missing=false; this is the assertion that it actually did.
func (p *parser) checkDecisionType(dt int64) error {
	if dt&1 != 0 {
		return fmt.Errorf("categorical split found; train with only numerical features")
	}
	if missing := (dt >> 2) & 3; missing != 0 {
		return fmt.Errorf("node uses missing-value handling (%d); train with use_missing=false and zero_as_missing=false", missing)
	}
	return nil
}

func (p *parser) parseSigmoid(objective string) {
	fields := strings.Fields(objective)
	if len(fields) == 0 {
		return
	}
	for _, part := range fields[1:] {
		if v, ok := strings.CutPrefix(part, "sigmoid:"); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				p.m.sigmoid = f
			}
		}
	}
}

func (p *parser) expectOne(key, value string) error {
	if value != "1" {
		return fmt.Errorf("%s=%s, only single-output models are supported", key, value)
	}
	return nil
}

func (p *parser) reset() {
	p.splitFeature, p.threshold, p.decisionType = nil, nil, nil
	p.leftChild, p.rightChild, p.leafValue = nil, nil, nil
	p.numLeaves = 0
}

func ints32(s string) ([]int32, error) {
	fields := strings.Fields(s)
	out := make([]int32, len(fields))
	for i, f := range fields {
		v, err := strconv.ParseInt(f, 10, 32)
		if err != nil {
			return nil, err
		}
		out[i] = int32(v)
	}
	return out, nil
}

func ints64(s string) ([]int64, error) {
	fields := strings.Fields(s)
	out := make([]int64, len(fields))
	for i, f := range fields {
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// floats32 parses at float64 precision and narrows. LightGBM writes thresholds with
// 17 significant digits, and parsing straight to float32 rounds differently than
// narrowing an exact double does.
//
// The comparison in Raw is then float32 against float32, where LightGBM's is double
// against double. Thresholds are midpoints between two observed feature values, so
// a feature would have to sit within a float32 epsilon of that midpoint to be
// routed differently, and the features here are counts and millisecond deltas.
// Halving the node array is worth that.
func floats32(s string) ([]float32, error) {
	fields := strings.Fields(s)
	out := make([]float32, len(fields))
	for i, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil, err
		}
		out[i] = float32(v)
	}
	return out, nil
}
