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
