package cache

import (
	"fmt"
	"testing"
)

// Overwriting or deleting then re-adding a key leaves its old slot in the queue.
// That stale slot must not be charged against smallBytes a second time.
func TestS3FIFOStaleSlotsKeepSmallBytesExact(t *testing.T) {
	c := newTestCache(t, 64<<10, 1, NewS3FIFO)
	for i := range 200 {
		key := fmt.Sprintf("hot-%d", i%8)
		c.Put(key, make([]byte, 256), 0)
		if i%3 == 0 {
			c.Delete(key)
			c.Put(key, make([]byte, 128), 0)
		}
		c.Get(key)
	}
	for i := range 2_000 {
		c.Admit(fmt.Sprintf("fill-%d", i), make([]byte, 512))
		c.Get(fmt.Sprintf("hot-%d", i%8))
	}
	assertAccounting(t, c)

	s := c.shards[0]
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.policy.(*s3fifo)
	var want int64
	for key := range p.inS {
		if e, ok := s.m[key]; ok {
			want += int64(e.size)
		}
	}
	if p.smallBytes != want {
		t.Fatalf("smallBytes = %d, entries in S total %d", p.smallBytes, want)
	}
}

type mapCandidates map[uint64]*Entry

func (m mapCandidates) Sample(int, func(*Entry) bool) {}
func (m mapCandidates) Len() int                      { return len(m) }
func (m mapCandidates) Lookup(key uint64) (*Entry, bool) {
	e, ok := m[key]
	return e, ok
}

// A key overwritten after promotion to M is promoted again from S, so M holds its
// old slot ahead of the new one. The old slot must be skipped, not treated as the
// key's position: otherwise the just-promoted entry is evicted ahead of j.
func TestS3FIFOSkipsTheOldSlotOfAReadmittedKey(t *testing.T) {
	p := NewS3FIFO(1 << 20).(*s3fifo)
	c := mapCandidates{}
	admit := func(key uint64) *Entry {
		e := &Entry{key: key, size: 100}
		c[key] = e
		p.OnAdmit(e)
		return e
	}
	const k, j = 1, 2

	admit(k).freq = 1
	p.stepSmall(c) // k promoted: M = [k]

	p.inG[j] = struct{}{}
	admit(j) // ghost hit, straight to M: M = [k, j]

	p.OnRemove(c[k])
	admit(k).freq = 1 // overwritten: S = [k]
	p.stepSmall(c)    // promoted again: M = [k(stale), j, k]

	if victim, ok := p.Victim(c, 0); !ok || victim != j {
		t.Fatalf("victim = %d, %v; want %d, the oldest live entry in M", victim, ok, j)
	}
}
