// Package trace samples the access stream a cache node serves and writes it where
// the trainer can find it.
//
// The shape of this is dictated by one constraint: nothing here may slow down a
// request. So the request path does a bounded, lock-free push into a per-shard ring
// and returns; a single background goroutine drains every ring, batches, and writes
// segment files. When the writer cannot keep up, records are dropped and counted.
//
// Segment files rather than a gRPC stream to the trainer, deliberately. Training runs
// minutes behind serving, so a streaming collector would add a network dependency,
// a backpressure question, and an availability risk to a pipeline whose whole point
// is that it is offline. A directory of finished files is also directly replayable,
// which is what makes a bad model reproducible instead of gone.
package trace

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	beladyv1 "github.com/JustinK33/newproj/gen/belady/v1"
)

// Extension marks a finished segment. Files are written under a temporary name and
// renamed on close, so a file with this suffix is always a complete sequence of
// length-delimited batches and the trainer never has to guess.
const Extension = ".trace"

const tmpExtension = ".partial"

type Config struct {
	Dir    string
	NodeID string

	Shards       int
	RingCapacity int
	// SampleDenominator keeps one key in every n. See Ring.Push for why the sampling
	// is by key and not by request.
	SampleDenominator int

	SegmentBytes  int64
	FlushInterval time.Duration
	// SegmentMaxAge rotates an open segment on age as well as on size. Without it a
	// node whose traffic never fills SegmentBytes publishes nothing until it shuts
	// down, and the trainer correctly reports an empty directory. Age is the bound
	// that matters to the trainer; size only bounds the file.
	SegmentMaxAge time.Duration
	BatchSize     int
}

type Recorder struct {
	log   *slog.Logger
	rings []*Ring

	// Writer state, touched only by Run.
	file    *os.File
	scratch []Record
	encoded []byte
	path    string
	opened  time.Time
	batch   beladyv1.AccessBatch

	cfg Config

	// written counts records that reached a segment file, which is the number the
	// trainer will actually see. It is not the ring's pushed count.
	written  atomic.Uint64
	size     int64
	sequence uint64
}

func New(cfg Config, log *slog.Logger) (*Recorder, error) {
	if cfg.Shards <= 0 {
		return nil, fmt.Errorf("trace: Shards must be positive")
	}
	if cfg.Dir == "" {
		return nil, fmt.Errorf("trace: Dir is required")
	}
	if cfg.RingCapacity <= 0 {
		cfg.RingCapacity = 8192
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 4096
	}
	if cfg.SegmentBytes <= 0 {
		cfg.SegmentBytes = 32 << 20
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	if cfg.SegmentMaxAge <= 0 {
		cfg.SegmentMaxAge = time.Minute
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("trace: %w", err)
	}

	r := &Recorder{
		rings:   make([]*Ring, cfg.Shards),
		log:     log,
		cfg:     cfg,
		scratch: make([]Record, cfg.BatchSize),
	}
	for i := range r.rings {
		r.rings[i] = NewRing(cfg.RingCapacity, cfg.SampleDenominator)
	}
	return r, nil
}

// Shards reports how many rings exist. The caller has to match this exactly:
// handing two shards the same ring would break the single-producer invariant the
// ring relies on, and it would do so silently.
func (r *Recorder) Shards() int { return len(r.rings) }

// Ring hands a shard its own ring. Called once per shard at construction.
func (r *Recorder) Ring(i int) *Ring { return r.rings[i] }

// Stats reports records written to disk and records dropped because a ring was full.
// Both are exported as metrics: a rising drop count is the signal that the sample
// rate or the ring is wrong, and it has to be visible before the model trained on
// that trace is trusted.
func (r *Recorder) Stats() (written, dropped uint64) {
	for _, ring := range r.rings {
		_, d := ring.counters()
		dropped += d
	}
	return r.written.Load(), dropped
}

// Run drains the rings until ctx is cancelled, then flushes and closes the open
// segment so no records are lost on a clean shutdown.
func (r *Recorder) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// One last pass: the rings almost certainly hold records accepted between
			// the final tick and the shutdown signal.
			if err := r.drainAll(); err != nil {
				r.log.Error("final trace flush failed", "err", err)
			}
			return r.closeSegment()
		case <-ticker.C:
			if err := r.drainAll(); err != nil {
				// A failed write must not kill the cache. Log it, keep draining, and let
				// the drop and written counters tell the operator the trace is incomplete.
				r.log.Error("trace write failed", "err", err)
			}
			if r.file != nil && time.Since(r.opened) >= r.cfg.SegmentMaxAge {
				if err := r.closeSegment(); err != nil {
					r.log.Error("trace segment rotation failed", "err", err)
				}
			}
		}
	}
}

func (r *Recorder) drainAll() error {
	for i, ring := range r.rings {
		for {
			n := ring.Drain(r.scratch)
			if n == 0 {
				break
			}
			if err := r.writeBatch(i, r.scratch[:n]); err != nil {
				return err
			}
			if n < len(r.scratch) {
				break
			}
		}
	}
	return nil
}

func (r *Recorder) writeBatch(shard int, recs []Record) error {
	// The batch and its record slice are reused across calls. Rebuilding the protobuf
	// messages every second is not free at a few hundred thousand records a second.
	if cap(r.batch.Records) < len(recs) {
		r.batch.Records = make([]*beladyv1.AccessRecord, len(recs))
		for i := range r.batch.Records {
			r.batch.Records[i] = &beladyv1.AccessRecord{}
		}
	}
	r.batch.Records = r.batch.Records[:len(recs)]
	for i, rec := range recs {
		if r.batch.Records[i] == nil {
			r.batch.Records[i] = &beladyv1.AccessRecord{}
		}
		out := r.batch.Records[i]
		out.KeyHash = rec.KeyHash
		out.TimestampUs = rec.TimestampUS
		out.SizeBytes = rec.SizeBytes
		out.Hit = rec.Hit
	}
	r.sequence++
	r.batch.NodeId = fmt.Sprintf("%s/%d", r.cfg.NodeID, shard)
	r.batch.Sequence = r.sequence

	encoded, err := proto.MarshalOptions{}.MarshalAppend(r.encoded[:0], &r.batch)
	if err != nil {
		return err
	}
	r.encoded = encoded

	if err := r.ensureSegment(); err != nil {
		return err
	}
	// Length-delimited framing: a varint byte count then the message. Protobuf is not
	// self-delimiting, so a file of concatenated messages cannot be split back apart
	// without this.
	frame := protowire.AppendVarint(nil, uint64(len(encoded)))
	if _, err := r.file.Write(frame); err != nil {
		return err
	}
	if _, err := r.file.Write(encoded); err != nil {
		return err
	}

	r.size += int64(len(frame) + len(encoded))
	r.written.Add(uint64(len(recs)))
	if r.size >= r.cfg.SegmentBytes {
		return r.closeSegment()
	}
	return nil
}

func (r *Recorder) ensureSegment() error {
	if r.file != nil {
		return nil
	}
	name := fmt.Sprintf("%s-%d", r.cfg.NodeID, time.Now().UnixNano())
	r.path = filepath.Join(r.cfg.Dir, name)
	f, err := os.OpenFile(r.path+tmpExtension, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	r.file, r.size, r.opened = f, 0, time.Now()
	return nil
}

// closeSegment publishes the segment by renaming it into place. A reader globbing for
// finished segments therefore never sees a partially written file, which on a local
// filesystem is guaranteed by rename being atomic.
func (r *Recorder) closeSegment() error {
	if r.file == nil {
		return nil
	}
	f, path, size := r.file, r.path, r.size
	r.file, r.path, r.size = nil, "", 0

	if err := f.Close(); err != nil {
		return err
	}
	if size == 0 {
		return os.Remove(path + tmpExtension)
	}
	if err := os.Rename(path+tmpExtension, path+Extension); err != nil {
		return err
	}
	r.log.Debug("trace segment written", "path", path+Extension, "bytes", size)
	return nil
}
