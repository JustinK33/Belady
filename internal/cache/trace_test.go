package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/JustinK33/Belady/internal/trace"
)

func newTraceCache(t *testing.T, shards int) (*Cache, *trace.Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	rec, err := trace.New(trace.Config{
		Dir:    dir,
		NodeID: "node-test",
		Shards: RoundShards(shards),
		// Keep every key: the assertions below count records exactly.
		SampleDenominator: 1,
		RingCapacity:      1 << 14,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	clk := &fakeClock{}
	c, err := New(Config{
		NewPolicy:     func(int64) Policy { return NewLRU(5) },
		NowUS:         clk.nowUS,
		Nanos:         func() int64 { return 0 },
		CapacityBytes: 1 << 20,
		Shards:        shards,
		Trace:         rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, rec, dir
}

// TestTraceRecordsExactlyOnePerRequest is the invariant the trainer depends on. A
// hit records from get and a miss records from insert; if both fired on a miss the
// labels would see a phantom re-access one microsecond after every admission, which
// is exactly the signal the model is being trained to predict.
func TestTraceRecordsExactlyOnePerRequest(t *testing.T) {
	c, rec, dir := newTraceCache(t, 8)

	const keys = 200
	for i := range keys {
		key := fmt.Sprintf("k%d", i)
		if _, ok := c.Get(key); ok {
			t.Fatalf("%s hit on an empty cache", key)
		}
		c.Admit(key, make([]byte, 64))
	}
	// Second pass: all hits.
	for i := range keys {
		if _, ok := c.Get(fmt.Sprintf("k%d", i)); !ok {
			t.Fatalf("k%d missed on the second pass", i)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rec.Run(ctx); err != nil {
		t.Fatal(err)
	}

	written, dropped := rec.Stats()
	if written != 2*keys || dropped != 0 {
		t.Fatalf("wrote %d records with %d dropped, want %d and 0", written, dropped, 2*keys)
	}

	batches, err := trace.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	hits, misses := 0, 0
	sizes := map[uint64]uint32{}
	for _, b := range batches {
		for _, r := range b.GetRecords() {
			if r.GetHit() {
				hits++
			} else {
				misses++
			}
			sizes[r.GetKeyHash()] = r.GetSizeBytes()
		}
	}
	if hits != keys || misses != keys {
		t.Errorf("recorded %d hits and %d misses, want %d of each", hits, misses, keys)
	}
	// A miss records from insert precisely so the size is known and never zero.
	for h, size := range sizes {
		if size != 64 {
			t.Fatalf("key %d recorded size %d, want 64", h, size)
		}
	}
}

// TestTraceRejectsAShardCountMismatch guards the single-producer invariant. Rings are
// per shard and lock-free on the push side; two shards sharing one ring corrupts the
// trace in a way that only shows up as a slightly wrong model months later.
func TestTraceRejectsAShardCountMismatch(t *testing.T) {
	rec, err := trace.New(trace.Config{
		Dir: t.TempDir(), NodeID: "n", Shards: 100, FlushInterval: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Config{
		NewPolicy:     func(int64) Policy { return NewLRU(5) },
		CapacityBytes: 1 << 20,
		Shards:        100, // rounds up to 128, so the recorder is 28 rings short
		Trace:         rec,
	})
	if err == nil {
		t.Fatal("a cache with 128 shards accepted a 100-ring recorder")
	}
}
