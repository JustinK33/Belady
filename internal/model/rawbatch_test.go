package model

// Prototypes for scoring a whole eviction sample in one pass, and the benchmark that
// decides whether any of them is worth putting in the hot path.
//
// The hypothesis: one tree walk is latency bound rather than compute bound, because
// each level loads the node and then the feature the node names, and the second
// address depends on the first. The eight candidates in an eviction sample are
// independent, so interleaving them should give the load unit several chains at once.
//
// The hypothesis is only partly plausible, which is why this file exists before any
// production change. BenchmarkRawVaryingFeatures reports 60 ns/op for 13 trees at
// depth 5, which is 65 node visits, so 0.9 ns per visit, roughly 4 cycles. A strictly
// serial two-load chain could not beat about 8 cycles per visit at any plausible L1
// latency, so Raw is already overlapping more than one chain: the trees inside one
// call are independent of each other and the out-of-order window spans several.
// Batching deepens parallelism that is partly present rather than creating it.
//
// This file is deleted once one variant wins, or reverted whole if none does.

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// protoSeq is the baseline: what the eviction path does today, one Raw per candidate.
// It has to be a real loop over distinct rows rather than the ns/op x 8 figure the
// other benchmarks derive, because the loop itself already gets some cross-call
// overlap and the derived figure understates the model by the documented 1.35x.
func protoSeq(m *Model, rows []float32, stride int, out []float32) {
	for c := range out {
		out[c] = m.Raw(rows[c*stride:])
	}
}

// protoTreeOuter swaps the loop nest: one tree, then every candidate through it. Each
// candidate is still walked to its leaf before the next starts, so this buys node
// locality and whatever overlap the out-of-order window finds between adjacent
// independent walks, and nothing more. It is six lines more than Raw.
func protoTreeOuter(m *Model, rows []float32, stride int, out []float32) {
	for c := range out {
		out[c] = 0
	}
	for _, root := range m.roots {
		for c := range out {
			f := rows[c*stride:]
			i := root
			for i >= 0 {
				n := &m.nodes[i]
				if f[n.feature] <= n.threshold {
					i = n.left
				} else {
					i = n.right
				}
			}
			out[c] += m.leaves[^i]
		}
	}
}

// protoLockstep advances every candidate by one level per pass, so the batch's node
// loads for a level are issued together instead of one after another. Walk state is a
// fixed-size local array, which caps the batch at protoWidth.
func protoLockstep(m *Model, rows []float32, stride int, out []float32) {
	var walk [protoWidth]int32
	live := walk[:len(out)]
	for c := range out {
		out[c] = 0
	}
	for _, root := range m.roots {
		for c := range live {
			live[c] = root
		}
		// A candidate that has reached its leaf holds a negative index and is skipped,
		// which is also how a tree whose root is already a leaf works with no special
		// case: no pass runs at all.
		for stepped := true; stepped; {
			stepped = false
			for c, i := range live {
				if i < 0 {
					continue
				}
				n := &m.nodes[i]
				if rows[c*stride+int(n.feature)] <= n.threshold {
					live[c] = n.left
				} else {
					live[c] = n.right
				}
				stepped = true
			}
		}
		for c, i := range live {
			out[c] += m.leaves[^i]
		}
	}
}

const protoWidth = 8

// protoUnroll4 is protoLockstep with the walk indices and the accumulators in scalar
// locals rather than arrays, which is the only shape that can keep both in registers.
// The loop guard is one AND chain: the sign bit of i0&i1&i2&i3 is set only when every
// candidate has reached a leaf, so four compares collapse to one.
func protoUnroll4(m *Model, rows []float32, stride int, out []float32) {
	c := 0
	for ; c+4 <= len(out); c += 4 {
		b0, b1, b2, b3 := c*stride, (c+1)*stride, (c+2)*stride, (c+3)*stride
		var s0, s1, s2, s3 float32
		for _, root := range m.roots {
			i0, i1, i2, i3 := root, root, root, root
			for i0&i1&i2&i3 >= 0 {
				if i0 >= 0 {
					n := &m.nodes[i0]
					if rows[b0+int(n.feature)] <= n.threshold {
						i0 = n.left
					} else {
						i0 = n.right
					}
				}
				if i1 >= 0 {
					n := &m.nodes[i1]
					if rows[b1+int(n.feature)] <= n.threshold {
						i1 = n.left
					} else {
						i1 = n.right
					}
				}
				if i2 >= 0 {
					n := &m.nodes[i2]
					if rows[b2+int(n.feature)] <= n.threshold {
						i2 = n.left
					} else {
						i2 = n.right
					}
				}
				if i3 >= 0 {
					n := &m.nodes[i3]
					if rows[b3+int(n.feature)] <= n.threshold {
						i3 = n.left
					} else {
						i3 = n.right
					}
				}
			}
			s0 += m.leaves[^i0]
			s1 += m.leaves[^i1]
			s2 += m.leaves[^i2]
			s3 += m.leaves[^i3]
		}
		out[c], out[c+1], out[c+2], out[c+3] = s0, s1, s2, s3
	}
	// Tail, which is also the short-sample case the eviction path hits on a shard
	// holding fewer entries than the sample size.
	for ; c < len(out); c++ {
		out[c] = m.Raw(rows[c*stride:])
	}
}

var protoVariants = []struct {
	name string
	fn   func(*Model, []float32, int, []float32)
}{
	{"seq", protoSeq},
	{"treeOuter", protoTreeOuter},
	{"lockstep", protoLockstep},
	{"unroll4", protoUnroll4},
}

// TestPrototypesAgreeWithRaw is not the shipped exactness test, it is the guard that
// makes the benchmark numbers mean anything: a variant that skipped work would look
// fast. Bit-exact, because every variant accumulates one leaf per tree over m.roots
// in order, exactly as Raw does, and there is no multiply so no FMA fusion is legal.
func TestPrototypesAgreeWithRaw(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	// stride wider than the model's feature count, so a variant that indexed rows by
	// numFeatures instead of stride fails here rather than in production.
	const features, stride = 12, 16
	text, _ := synth(40, 5, features, rng)
	m, err := Load(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range protoVariants {
		t.Run(v.name, func(t *testing.T) {
			for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8} {
				rows := make([]float32, n*stride)
				for i := range rows {
					rows[i] = float32(rng.Float64() * 100)
				}
				out := make([]float32, n)
				v.fn(m, rows, stride, out)
				for c := range out {
					if want := m.Raw(rows[c*stride:]); out[c] != want {
						t.Fatalf("n=%d out[%d] = %v, Raw = %v", n, c, out[c], want)
					}
				}
			}
		})
	}
}

// BenchmarkRawBatchVectorPool asks whether the 13-tree figure is trustworthy.
//
// seq costs 0.94 ns per node visit at 13 trees and 3.45 ns at 50, on the same
// vectors, which is too big a spread to be cache capacity: 50 trees of 31 nodes is
// 24.8 KB of nodes and the M4's L1d is 128 KB. The other candidate is that with only
// 512 distinct vectors cycled, 13 trees is 512 x 13 paths, which the branch predictor
// can still memorise, and 41 trees is not. If that is what is happening then the
// 13-tree number is optimistically fast for a reason that has nothing to do with the
// live cache, where no path repeats.
//
// Growing the pool confounds branches with cache footprint, since 65536 vectors is
// 4 MB of rows. The reads are sequential, so a prefetcher should cover most of it,
// but the effect this measures is an upper bound on the branch component rather than
// a clean isolation of it.
func BenchmarkRawBatchVectorPool(b *testing.B) {
	const (
		stride = 16
		batch  = 8
	)
	rng := rand.New(rand.NewPCG(9, 1))
	text, _ := synth(13, 5, stride, rng)
	m, err := Load(strings.NewReader(text))
	if err != nil {
		b.Fatal(err)
	}
	out := make([]float32, batch)

	for _, vectors := range []int{512, 1024, 2048, 4096, 8192, 32768, 262144} {
		flat := make([]float32, vectors*stride)
		for i := range flat {
			flat[i] = float32(rng.Float64() * 100)
		}
		for _, v := range protoVariants {
			if v.name == "treeOuter" || v.name == "lockstep" {
				continue
			}
			b.Run(fmt.Sprintf("vectors=%d/%s", vectors, v.name), func(b *testing.B) {
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					v.fn(m, flat[i:i+batch*stride], stride, out)
					if i += batch * stride; i+batch*stride > len(flat) {
						i = 0
					}
				}
				perOp := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
				b.ReportMetric(perOp/65, "ns/nodevisit")
				keepAlive(b, m, out[0])
			})
		}
	}
}

// BenchmarkRawBatch is the decision. Compare each variant against seq at the same
// model shape and batch size, on rows that are never hot, which is the only thing the
// live cache ever does.
func BenchmarkRawBatch(b *testing.B) {
	const (
		stride  = 16
		vectors = 512
	)
	// 13 is what the trainer fits and what runs live. 41 is the other tree count the
	// same workload produced minutes later, so it is a real shape and not a
	// hypothetical. 25 and 33 locate the crossover between them; 50 and 100 are there
	// to show which direction the effect runs.
	for _, cfg := range []struct{ trees, depth int }{{13, 5}, {25, 5}, {33, 5}, {41, 5}, {50, 5}, {100, 6}} {
		rng := rand.New(rand.NewPCG(9, 1))
		text, _ := synth(cfg.trees, cfg.depth, stride, rng)
		m, err := Load(strings.NewReader(text))
		if err != nil {
			b.Fatal(err)
		}
		// One flat backing array cycled across iterations, so walking the set is a
		// sequential read and does not import a cache-miss effect.
		flat := make([]float32, vectors*stride)
		for i := range flat {
			flat[i] = float32(rng.Float64() * 100)
		}

		for _, batch := range []int{8, 3} {
			out := make([]float32, batch)
			for _, v := range protoVariants {
				name := fmt.Sprintf("trees=%d/batch=%d/%s", cfg.trees, batch, v.name)
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					i := 0
					for b.Loop() {
						v.fn(m, flat[i:i+batch*stride], stride, out)
						if i += batch * stride; i+batch*stride > len(flat) {
							i = 0
						}
					}
					// Scaled to a sample of 8 so every row of the table compares.
					perOp := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
					b.ReportMetric(perOp*8/float64(batch), "ns/eviction")
					keepAlive(b, m, out[0])
				})
			}
		}
	}
}
