// Package features defines the feature vector shared by training and serving.
//
// This package is the contract. The Python trainer computes the same columns in the
// same order from a recorded trace, and the cache computes them from live entries at
// eviction time. If the two ever disagree the model still loads, still runs, and
// silently predicts nonsense, so the layout lives in exactly one place and both
// sides are checked against it: Names is emitted into the model metadata at training
// time and compared on load.
//
// Every value is a float32 and no value is ever NaN, which is what lets the
// evaluator in internal/model be a plain two-way branch per node. See
// docs/02-learned-eviction.md for why each column is here.
package features

import "strconv"

// HistoryLen is how many past inter-arrival gaps each entry remembers.
//
// The Relaxed Belady paper uses 32. Eight covers the same shape of signal at a
// quarter of the per-object overhead, which matters when the metadata is competing
// with cached bytes for the same memory budget.
// ponytail: 8 deltas. Widen it if feature importance shows the oldest delta still
// carrying weight.
const HistoryLen = 8

// Column indices. Ordered so the cheap scalars come first and the delta history is
// one contiguous run, which is also the order Names lists them in.
const (
	SizeBytes = iota
	RecencyMS
	AgeMS
	Accesses
	Frequency
	ReuseRate
	Delta0 // Delta0..Delta0+HistoryLen-1

	Count = Delta0 + HistoryLen
)

// Names must match the trainer's column names exactly and in order.
var Names = buildNames()

func buildNames() []string {
	n := make([]string, Count)
	n[SizeBytes] = "size_bytes"
	n[RecencyMS] = "recency_ms"
	n[AgeMS] = "age_ms"
	n[Accesses] = "accesses"
	n[Frequency] = "frequency"
	n[ReuseRate] = "reuse_rate"
	for i := range HistoryLen {
		n[Delta0+i] = "delta_" + strconv.Itoa(i)
	}
	return n
}

// Input is what the cache knows about one eviction candidate. It is passed by
// pointer and reused across the candidates of a single eviction, so extraction
// allocates nothing.
type Input struct {
	// Deltas holds gaps between consecutive past accesses in milliseconds, newest
	// first, zero where there is no history yet.
	Deltas *[HistoryLen]uint32

	NowUS        int64
	LastAccessUS int64
	AdmittedUS   int64

	SizeBytes int32
	Accesses  uint32
	Frequency uint8
}

// Extract fills dst, which must have at least Count entries.
//
// Milliseconds rather than seconds because a process-local cache sees reuse well
// inside one second, and a float32 holds every integer millisecond exactly out to
// about 4.6 hours. Past that the value rounds, which cannot move a threshold split
// that is already measured in hours.
func Extract(dst []float32, in *Input) {
	recency := in.NowUS - in.LastAccessUS
	age := in.NowUS - in.AdmittedUS
	if recency < 0 {
		recency = 0
	}
	if age < 0 {
		age = 0
	}

	dst[SizeBytes] = float32(in.SizeBytes)
	dst[RecencyMS] = float32(recency / 1000)
	dst[AgeMS] = float32(age / 1000)
	dst[Accesses] = float32(in.Accesses)
	dst[Frequency] = float32(in.Frequency)

	// Accesses per second while resident. A tree can threshold on accesses and on age
	// independently but cannot form their ratio, so this is the one column that is not
	// a monotone function of something already present.
	dst[ReuseRate] = float32(in.Accesses) / (float32(age)/1e6 + 1)

	for i := range HistoryLen {
		dst[Delta0+i] = float32(in.Deltas[i])
	}
}
