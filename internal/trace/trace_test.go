package trace

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRingCapacityRoundsUpToAPowerOfTwo pins the sizing, which every other ring
// test takes for granted by passing an exact power of two. The result becomes a
// mask, so a size that is not a power of two does not fail loudly: Push writes at
// tail&mask into a buffer of a different length and records overwrite each other
// while the counters report success.
func TestRingCapacityRoundsUpToAPowerOfTwo(t *testing.T) {
	// Including the degenerate inputs NewRing clamps, so the rounding is exercised
	// rather than hidden behind the clamp.
	for _, c := range []struct{ in, want int }{
		{-1, 2}, {0, 2}, {1, 2}, {2, 2}, {3, 4}, {5, 8}, {1000, 1024}, {1 << 16, 1 << 16},
	} {
		r := NewRing(c.in, 1)
		if len(r.buf) != c.want {
			t.Errorf("NewRing(%d) sized %d, want %d", c.in, len(r.buf), c.want)
		}
		if r.mask != uint64(len(r.buf)-1) {
			t.Errorf("NewRing(%d) mask = %d, want %d: mask and length must agree", c.in, r.mask, len(r.buf)-1)
		}
	}

	// A capacity that had to be rounded still holds every record it accepts.
	r := NewRing(5, 1)
	for i := range 8 {
		if !r.Push(Record{KeyHash: uint64(i)}) {
			t.Fatalf("push %d refused at a rounded capacity of %d", i, len(r.buf))
		}
	}
	out := make([]Record, 8)
	if n := r.Drain(out); n != 8 {
		t.Fatalf("drained %d records, want 8", n)
	}
	for i := range 8 {
		if out[i].KeyHash != uint64(i) {
			t.Errorf("record %d has key %d: the mask and the buffer disagree", i, out[i].KeyHash)
		}
	}
}

func TestRingRoundTrip(t *testing.T) {
	r := NewRing(8, 1)
	for i := range 5 {
		if !r.Push(Record{KeyHash: uint64(i), TimestampUS: int64(i) * 10, SizeBytes: uint32(i), Hit: i%2 == 0}) {
			t.Fatalf("push %d was refused with room to spare", i)
		}
	}

	out := make([]Record, 16)
	if n := r.Drain(out); n != 5 {
		t.Fatalf("drained %d records, want 5", n)
	}
	for i := range 5 {
		want := Record{KeyHash: uint64(i), TimestampUS: int64(i) * 10, SizeBytes: uint32(i), Hit: i%2 == 0}
		if out[i] != want {
			t.Errorf("record %d = %+v, want %+v", i, out[i], want)
		}
	}
	if n := r.Drain(out); n != 0 {
		t.Errorf("drained %d records from an empty ring", n)
	}
}

func TestRingDropsWhenFullRatherThanBlocking(t *testing.T) {
	r := NewRing(4, 1)
	accepted := 0
	for i := range 100 {
		if r.Push(Record{KeyHash: uint64(i)}) {
			accepted++
		}
	}

	pushed, dropped := r.counters()
	if accepted != 4 || pushed != 4 {
		t.Errorf("accepted %d and counted %d pushes into a 4-slot ring", accepted, pushed)
	}
	if pushed+dropped != 100 {
		t.Errorf("%d pushed plus %d dropped, want 100 accounted for", pushed, dropped)
	}

	// The four kept must be the four oldest, not a scrambled selection.
	out := make([]Record, 8)
	n := r.Drain(out)
	for i := range n {
		if out[i].KeyHash != uint64(i) {
			t.Errorf("slot %d holds key %d, want %d", i, out[i].KeyHash, i)
		}
	}
}

// TestRingCapacityIsReusable catches the classic ring bug where the cursors are
// compared with a modulo instead of by difference, so the buffer can only ever be
// filled once.
func TestRingCapacityIsReusable(t *testing.T) {
	r := NewRing(4, 1)
	out := make([]Record, 4)

	for round := range 100 {
		for i := range 4 {
			if !r.Push(Record{KeyHash: uint64(round*4 + i)}) {
				t.Fatalf("round %d: push %d refused on an empty ring", round, i)
			}
		}
		if n := r.Drain(out); n != 4 {
			t.Fatalf("round %d: drained %d, want 4", round, n)
		}
	}
	if _, dropped := r.counters(); dropped != 0 {
		t.Errorf("%d records dropped despite the ring being emptied every round", dropped)
	}
}

func TestRingSamplesByKey(t *testing.T) {
	const denom = 8
	r := NewRing(1<<16, denom)

	// Every access to a sampled key must be kept, which is the property labelling
	// depends on. Push each key three times and check the count comes out as a
	// multiple of three.
	kept := map[uint64]int{}
	for key := range uint64(4000) {
		for range 3 {
			if r.Push(Record{KeyHash: key << 32}) {
				kept[key]++
			}
		}
	}
	for key, n := range kept {
		if n != 3 {
			t.Fatalf("key %d kept %d of 3 accesses: sampling is per request, not per key", key, n)
		}
	}

	// Roughly one key in eight, with slack for the hash-independent test keys used
	// here being exactly divisible.
	if len(kept) < 300 || len(kept) > 700 {
		t.Errorf("kept %d of 4000 keys, want about %d", len(kept), 4000/denom)
	}
}

// TestRingIsRaceFreeAcrossGoroutines is the test the -race flag makes meaningful. The
// producer and consumer share no lock, so if the release store publishing the tail
// were a plain write, this is where it would show up.
func TestRingIsRaceFreeAcrossGoroutines(t *testing.T) {
	const total = 200_000
	r := NewRing(1024, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range total {
			// Spin rather than drop, so the assertion below can be exact.
			for !r.Push(Record{KeyHash: uint64(i)}) {
			}
		}
	}()

	out := make([]Record, 256)
	next := uint64(0)
	for next < total {
		n := r.Drain(out)
		for i := range n {
			if out[i].KeyHash != next {
				t.Fatalf("received key %d, expected %d: the ring reordered or duplicated a record",
					out[i].KeyHash, next)
			}
			next++
		}
	}
	<-done
}

func newRecorder(t *testing.T, cfg Config) *Recorder {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	if cfg.NodeID == "" {
		cfg.NodeID = "node-test"
	}
	if cfg.Shards == 0 {
		cfg.Shards = 4
	}
	r, err := New(cfg, discard())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecorderWritesReadableSegments(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, Config{Dir: dir, Shards: 4, FlushInterval: 5 * time.Millisecond})

	const perShard = 500
	for shard := range 4 {
		for i := range perShard {
			if !r.Ring(shard).Push(Record{
				KeyHash: uint64(shard*perShard + i), TimestampUS: int64(i), SizeBytes: 100, Hit: i%2 == 0,
			}) {
				t.Fatalf("shard %d push %d dropped", shard, i)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run does one final drain before returning, which is what is under test.
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}

	written, dropped := r.Stats()
	if written != 4*perShard || dropped != 0 {
		t.Fatalf("wrote %d records with %d dropped, want %d and 0", written, dropped, 4*perShard)
	}

	batches, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	seen := map[uint64]bool{}
	for _, b := range batches {
		if b.GetNodeId() == "" {
			t.Error("a batch carries no node id, so its records cannot be attributed")
		}
		for _, rec := range b.GetRecords() {
			if seen[rec.GetKeyHash()] {
				t.Fatalf("key %d appears twice", rec.GetKeyHash())
			}
			seen[rec.GetKeyHash()] = true
			if rec.GetSizeBytes() != 100 {
				t.Fatalf("key %d recorded size %d", rec.GetKeyHash(), rec.GetSizeBytes())
			}
			count++
		}
	}
	if count != 4*perShard {
		t.Errorf("read back %d records, want %d", count, 4*perShard)
	}
}

// TestRecorderPublishesOnlyFinishedSegments matters because the trainer globs this
// directory while the cache is still writing to it. A partially written file that
// matched the glob would fail to parse, and the obvious "just skip corrupt files"
// workaround would silently drop good data too.
func TestRecorderPublishesOnlyFinishedSegments(t *testing.T) {
	dir := t.TempDir()
	// A tiny segment limit forces several rotations.
	r := newRecorder(t, Config{Dir: dir, Shards: 2, SegmentBytes: 2 << 10, BatchSize: 64})

	for shard := range 2 {
		for i := range 2000 {
			r.Ring(shard).Push(Record{KeyHash: uint64(shard)<<20 | uint64(i), SizeBytes: 64})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != Extension {
			t.Errorf("%s was left behind unpublished", e.Name())
		}
	}

	segments, err := Segments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) < 2 {
		t.Errorf("got %d segments, expected rotation to produce several", len(segments))
	}

	written, _ := r.Stats()
	batches, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var count uint64
	for _, b := range batches {
		count += uint64(len(b.GetRecords()))
	}
	if count != written {
		t.Errorf("segments hold %d records but the recorder counted %d written", count, written)
	}
}

// TestRecorderRotatesOnAge covers the failure mode that a segment sized for a busy
// node never rotates on a quiet one, so the trainer sees a directory holding nothing
// but an open .partial file and reports it as empty.
func TestRecorderRotatesOnAge(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, Config{
		Dir:           dir,
		Shards:        1,
		SegmentBytes:  1 << 30, // Far beyond what the handful of records below can fill.
		FlushInterval: 5 * time.Millisecond,
		SegmentMaxAge: 20 * time.Millisecond,
	})

	for i := range 10 {
		r.Ring(0).Push(Record{KeyHash: uint64(i), SizeBytes: 64})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for {
		segments, err := Segments(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no segment was published, so age-based rotation is not happening")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestNewRefusesAnUnwritableDirectory is the container case: a fresh Docker volume is
// owned by root, so a node running as uid 65532 could otherwise start, serve, and log
// a write failure every flush interval for the rest of its life.
func TestNewRefusesAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	dir := filepath.Join(t.TempDir(), "traces")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Dir: dir, Shards: 1}, discard()); err == nil {
		t.Error("a recorder was built for a directory it cannot write to")
	}
}

func TestReadDirRejectsAnEmptyDirectory(t *testing.T) {
	if _, err := ReadDir(t.TempDir()); err == nil {
		t.Error("reading a directory with no segments succeeded")
	}
}

func BenchmarkRingPush(b *testing.B) {
	// Sized so the consumer below never lets it fill, isolating the push cost.
	r := NewRing(1<<16, 1)
	stop := make(chan struct{})
	go func() {
		out := make([]Record, 4096)
		for {
			select {
			case <-stop:
				return
			default:
				r.Drain(out)
			}
		}
	}()
	defer close(stop)

	b.ReportAllocs()
	var i uint64
	for b.Loop() {
		r.Push(Record{KeyHash: i, TimestampUS: int64(i), SizeBytes: 512})
		i++
	}
}
