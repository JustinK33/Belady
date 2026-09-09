package features

import (
	"strings"
	"testing"
)

func TestNamesMatchTheLayout(t *testing.T) {
	if len(Names) != Count {
		t.Fatalf("%d names for %d columns", len(Names), Count)
	}
	seen := map[string]bool{}
	for i, n := range Names {
		if n == "" {
			t.Errorf("column %d has no name", i)
		}
		if seen[n] {
			t.Errorf("duplicate column name %q", n)
		}
		seen[n] = true
	}
	// The trainer reads these off the model file to pair columns with the Go layout,
	// so a name with a space in it would silently split into two columns.
	for _, n := range Names {
		if strings.ContainsAny(n, " \t,") {
			t.Errorf("column name %q contains whitespace or a comma", n)
		}
	}
}

func TestExtract(t *testing.T) {
	deltas := [HistoryLen]uint32{500, 250, 125, 0, 0, 0, 0, 0}
	in := Input{
		Deltas:       &deltas,
		NowUS:        10_000_000, // 10s
		LastAccessUS: 9_500_000,  // 500ms ago
		AdmittedUS:   2_000_000,  // 8s of age
		SizeBytes:    4096,
		Accesses:     16,
		Frequency:    3,
	}
	dst := make([]float32, Count)
	Extract(dst, &in)

	want := map[int]float32{
		SizeBytes:  4096,
		RecencyMS:  500,
		AgeMS:      8000,
		Accesses:   16,
		Frequency:  3,
		ReuseRate:  16.0 / 9.0, // 16 accesses over 8s of age, plus the 1s offset
		Delta0:     500,
		Delta0 + 1: 250,
		Delta0 + 2: 125,
		Delta0 + 3: 0,
	}
	for col, w := range want {
		if got := dst[col]; got != w {
			t.Errorf("%s = %v, want %v", Names[col], got, w)
		}
	}
}

// TestExtractHandlesClockGoingBackwards matters because the cache reads a coarse
// clock: two goroutines can observe timestamps out of order, and a negative age
// would make reuse_rate negative or infinite and put the candidate on the wrong side
// of every split trained on it.
func TestExtractHandlesClockGoingBackwards(t *testing.T) {
	var deltas [HistoryLen]uint32
	in := Input{Deltas: &deltas, NowUS: 1000, LastAccessUS: 5000, AdmittedUS: 9000, Accesses: 4}
	dst := make([]float32, Count)
	Extract(dst, &in)

	if dst[RecencyMS] != 0 || dst[AgeMS] != 0 {
		t.Errorf("recency %v age %v, both should clamp to 0", dst[RecencyMS], dst[AgeMS])
	}
	if dst[ReuseRate] != 4 {
		t.Errorf("reuse_rate = %v, want 4 with age clamped to zero", dst[ReuseRate])
	}
}

func TestExtractDoesNotAllocate(t *testing.T) {
	var deltas [HistoryLen]uint32
	in := Input{Deltas: &deltas, NowUS: 1 << 40, Accesses: 7, SizeBytes: 1024}
	dst := make([]float32, Count)

	if n := testing.AllocsPerRun(100, func() { Extract(dst, &in) }); n != 0 {
		t.Errorf("Extract allocated %v times per call", n)
	}
}
