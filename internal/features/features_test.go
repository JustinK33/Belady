package features

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// TestNamesMatchTheTrainer is the only guard against the two halves of the feature
// contract drifting apart. The trainer cannot import this package and this package
// cannot import the trainer, so the check is a parse of the Python source: crude, but
// it fails the build, which no amount of documentation does.
//
// The failure it prevents is silent. Swap two columns and the model still loads, still
// evaluates, and reads size_bytes wherever it was trained to read recency_ms.
func TestNamesMatchTheTrainer(t *testing.T) {
	path := filepath.Join("..", "..", "trainer", "belady_trainer", "columns.py")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the trainer's column layout is unreadable, so the two sides cannot be compared: %v", err)
	}

	lenMatch := regexp.MustCompile(`(?m)^HISTORY_LEN = (\d+)$`).FindSubmatch(src)
	if lenMatch == nil {
		t.Fatalf("no HISTORY_LEN assignment in %s", path)
	}
	historyLen, err := strconv.Atoi(string(lenMatch[1]))
	if err != nil {
		t.Fatal(err)
	}
	if historyLen != HistoryLen {
		t.Errorf("HISTORY_LEN is %d in the trainer and %d here", historyLen, HistoryLen)
	}

	block := regexp.MustCompile(`(?s)\nNAMES = \[(.*?)\n\]`).FindSubmatch(src)
	if block == nil {
		t.Fatalf("no NAMES list in %s", path)
	}

	// The trainer spells the scalar columns as plain string literals and generates the
	// delta history from HISTORY_LEN, so rebuild the list the same way rather than
	// trying to evaluate Python.
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^\s+"([a-z0-9_]+)",$`).FindAllSubmatch(block[1], -1) {
		names = append(names, string(m[1]))
	}
	if !strings.Contains(string(block[1]), `f"delta_{i}" for i in range(HISTORY_LEN)`) {
		t.Fatalf("NAMES no longer generates the delta history from HISTORY_LEN:\n%s", block[1])
	}
	for i := range historyLen {
		names = append(names, "delta_"+strconv.Itoa(i))
	}

	if len(names) != len(Names) {
		t.Fatalf("the trainer declares %d columns %v, this package declares %d %v",
			len(names), names, len(Names), Names)
	}
	for i := range Names {
		if names[i] != Names[i] {
			t.Errorf("column %d is %q in the trainer and %q here", i, names[i], Names[i])
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
