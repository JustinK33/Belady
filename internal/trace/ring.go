package trace

import "sync/atomic"

// Record is one access, and is deliberately 24 bytes with no pointers: the ring is
// an array of these, so the drain loop is a straight memory copy and the garbage
// collector never has to scan it.
type Record struct {
	KeyHash     uint64
	TimestampUS int64
	SizeBytes   uint32
	Hit         bool
}

// cacheLine is the padding unit. Sharing a line between the producer's and
// consumer's cursors is the classic false-sharing bug in a ring buffer: neither side
// reads the other's counter often, but every store invalidates the line in the other
// core's cache, so the two ends of an otherwise contention-free structure end up
// ping-ponging one line.
const cacheLine = 64

// Ring is a bounded single-producer single-consumer queue.
//
// Single-producer is not an assumption, it is enforced by where this lives: one Ring
// belongs to one cache shard and is only ever pushed to with that shard's lock held,
// so pushes to a given ring are already serialised. The consumer is the recorder's
// one drain goroutine.
//
// Full means drop, never block and never grow. The request path's budget is
// microseconds and the trainer runs minutes behind, so a stalled shipper must cost
// sampling fidelity rather than latency. Drops are counted and exported, because a
// silently truncated trace trains a model that looks fine and is not.
type Ring struct {
	buf []Record

	mask       uint64
	sampleMask uint64

	// Producer side. cachedHead is this side's possibly-stale view of the consumer's
	// cursor, refreshed only when the ring looks full, so a push in the common case
	// touches no cache line the consumer writes.
	tail       atomic.Uint64
	cachedHead uint64
	pushed     atomic.Uint64
	drops      atomic.Uint64
	_          [cacheLine]byte

	// Consumer side.
	head atomic.Uint64
	_    [cacheLine]byte
}

// NewRing allocates a ring holding capacity records, rounded up to a power of two.
// sampleDenominator keeps one key in every n, also rounded up to a power of two; 1
// keeps everything.
func NewRing(capacity, sampleDenominator int) *Ring {
	if capacity < 2 {
		capacity = 2
	}
	if sampleDenominator < 1 {
		sampleDenominator = 1
	}
	return &Ring{
		buf:        make([]Record, roundUpPow2(capacity)),
		mask:       uint64(roundUpPow2(capacity) - 1),
		sampleMask: uint64(roundUpPow2(sampleDenominator) - 1),
	}
}

// Push enqueues a record, returning false if it was dropped or not sampled.
//
// Sampling is by key rather than by request: every access to a chosen key is kept,
// and keys not chosen are ignored entirely. That is what Relaxed Belady labelling
// needs, because a label is "when is this object next requested" and a trace with
// every tenth request of every key answers that wrongly for all of them, while a
// trace with every request of every tenth key answers it exactly.
//
// The decision reads the high bits of the hash. The low bits already select the
// shard, so sampling on them would confine the sample to a fraction of the shards.
func (r *Ring) Push(rec Record) bool {
	if (rec.KeyHash>>32)&r.sampleMask != 0 {
		return false
	}

	tail := r.tail.Load()
	if tail-r.cachedHead >= uint64(len(r.buf)) {
		r.cachedHead = r.head.Load()
		if tail-r.cachedHead >= uint64(len(r.buf)) {
			r.drops.Add(1)
			return false
		}
	}

	r.buf[tail&r.mask] = rec
	// The release semantics of this store are what publish the record above: a
	// consumer that observes the new tail is guaranteed to see the slot written.
	r.tail.Store(tail + 1)
	r.pushed.Add(1)
	return true
}

// Drain moves up to len(dst) records out and returns how many.
func (r *Ring) Drain(dst []Record) int {
	head := r.head.Load()
	n := int(r.tail.Load() - head)
	if n <= 0 {
		return 0
	}
	if n > len(dst) {
		n = len(dst)
	}
	for i := range n {
		dst[i] = r.buf[(head+uint64(i))&r.mask]
	}
	r.head.Store(head + uint64(n))
	return n
}

func (r *Ring) counters() (pushed, dropped uint64) {
	return r.pushed.Load(), r.drops.Load()
}

func roundUpPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
