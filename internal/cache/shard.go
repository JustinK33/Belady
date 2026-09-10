package cache

import (
	"sync"

	"github.com/JustinK33/Belady/internal/trace"
)

// shard is an independently locked slice of the keyspace. Everything that
// matters for concurrency happens here: there is no cache-wide lock, and no
// cache-wide counter, so two requests touching different shards share nothing
// but the shard slice header.
//
// The lock is a plain Mutex, not an RWMutex. A hit mutates the entry's access
// history, so readers need exclusive access anyway, and RWMutex costs more than
// Mutex when every acquisition is a write.
type shard struct {
	m      map[uint64]*Entry
	policy Policy
	nanos  func() int64 // monotonic nanoseconds, injectable so tests stay fast

	// ring is this shard's slice of the access trace, or nil when tracing is off.
	// Pushing here is why the ring can be single-producer: the shard lock is already
	// held, so no two goroutines are ever in it at once.
	ring *trace.Ring

	used     int64
	capacity int64
	sample   int

	// Counters live here rather than in the Cache so the hot path never touches a
	// shared atomic. They are guarded by mu, which the caller already holds.
	hits, misses         uint64
	hitBytes, missBytes  uint64
	admissions, rejects  uint64
	evictions            uint64
	expirations          uint64
	evictNS, evictSample uint64

	mu sync.Mutex
}

func newShard(capacity int64, sample int, policy Policy, nanos func() int64, ring *trace.Ring) *shard {
	return &shard{
		m:        make(map[uint64]*Entry),
		policy:   policy,
		nanos:    nanos,
		ring:     ring,
		capacity: capacity,
		sample:   sample,
	}
}

// record appends to the access trace. Exactly one record is written per request: a
// hit records here, and a miss records from insert once the object's size is known,
// so the trainer sees every access with a real size and never a duplicate.
func (s *shard) record(key uint64, nowUS int64, size uint32, hit bool) {
	if s.ring == nil {
		return
	}
	s.ring.Push(trace.Record{KeyHash: key, TimestampUS: nowUS, SizeBytes: size, Hit: hit})
}

// Sample walks a randomised subset of the shard. Go randomises the starting
// bucket of every range over a map, which is enough entropy to pick eviction
// candidates without maintaining a second index over the keyspace.
// ponytail: the sample skews toward entries in fuller buckets. A reservoir over a
// flat entry slice would be unbiased; do that only if a policy's measured hit
// ratio turns out to be sampling-limited.
func (s *shard) Sample(n int, fn func(*Entry) bool) {
	if n <= 0 {
		return
	}
	for _, e := range s.m {
		if !fn(e) {
			return
		}
		if n--; n == 0 {
			return
		}
	}
}

func (s *shard) Lookup(key uint64) (*Entry, bool) {
	e, ok := s.m[key]
	return e, ok
}

func (s *shard) Len() int { return len(s.m) }

func (s *shard) get(key uint64, nowUS int64) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.m[key]
	if !ok {
		s.misses++
		return nil, false
	}
	// Expiry is resolved here rather than by a sweeper, so a stale entry is never
	// served and the read that would have hit pays for the cleanup.
	if e.expired(nowUS) {
		s.drop(key, e)
		s.expirations++
		s.misses++
		return nil, false
	}
	e.touch(nowUS)
	s.policy.OnAccess(e)
	s.hits++
	s.hitBytes += uint64(e.size)
	s.record(key, nowUS, uint32(e.size), true)
	return e.value, true
}

// drop removes an entry and keeps the byte accounting and the policy in step. The
// caller holds the lock and owns whichever counter the removal should be charged to.
func (s *shard) drop(key uint64, e *Entry) {
	delete(s.m, key)
	s.used -= int64(e.size)
	s.policy.OnRemove(e)
}

// insert stores value, evicting as needed. onMiss marks the insert as an
// admission following a cache miss, which is what byte hit ratio is measured
// against; a client-driven write is not a miss. expires is a microsecond deadline,
// or 0 for no TTL.
func (s *shard) insert(key uint64, value []byte, nowUS, expires int64, onMiss bool) bool {
	size := int64(len(value))

	s.mu.Lock()
	defer s.mu.Unlock()

	if onMiss {
		s.missBytes += uint64(size)
	}

	// An object that cannot fit even in an empty shard is never worth thrashing
	// the whole shard for.
	if size > s.capacity {
		s.rejects++
		return false
	}

	if old, ok := s.m[key]; ok {
		s.drop(key, old)
	}

	for s.used+size > s.capacity {
		if !s.evictOne(nowUS) {
			s.rejects++
			return false
		}
	}

	e := &Entry{
		value:      value,
		key:        key,
		lastAccess: nowUS,
		admitted:   nowUS,
		expires:    expires,
		size:       int32(size),
		accesses:   1,
		freq:       1,
	}
	s.m[key] = e
	s.used += size
	s.policy.OnAdmit(e)
	s.admissions++
	if onMiss {
		s.record(key, nowUS, uint32(size), false)
	}
	return true
}

// evictOne asks the policy for a victim and drops it. It returns false when no
// victim could be produced, which stops the caller's eviction loop from spinning.
func (s *shard) evictOne(nowUS int64) bool {
	start := s.nanos()
	victim, ok := s.policy.Victim(s, nowUS)
	s.evictNS += uint64(s.nanos() - start)
	s.evictSample++

	if !ok {
		return false
	}
	e, ok := s.m[victim]
	if !ok {
		// A policy that names a key the shard does not hold is a bug in the
		// policy, but eviction must still terminate.
		return false
	}
	s.drop(victim, e)
	s.evictions++
	return true
}

func (s *shard) remove(key uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.m[key]
	if !ok {
		return false
	}
	s.drop(key, e)
	return true
}
