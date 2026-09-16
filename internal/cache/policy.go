package cache

import "math"

// Policy decides which object leaves when a shard is full. Every method is
// called with the owning shard's lock already held, so implementations must not
// lock, block, or allocate.
type Policy interface {
	Name() string

	// Victim returns the key to evict. Returning false means the policy declined,
	// which the shard treats as "cannot make room".
	Victim(c Candidates, nowUS int64) (key uint64, ok bool)

	// The hooks exist for queue-based policies that maintain their own ordering.
	// Sampled policies leave them empty.
	OnAccess(e *Entry)
	OnAdmit(e *Entry)
	OnRemove(e *Entry)
}

// Candidates is a shard's read interface for a policy.
type Candidates interface {
	// Sample visits up to n randomly chosen entries, stopping early if fn returns
	// false.
	Sample(n int, fn func(*Entry) bool)
	Lookup(key uint64) (*Entry, bool)
	Len() int
}

// sampled is the shared body of LRU, LFU and the learned policy: draw a few
// random candidates, score each, evict the highest score. Sampling is what makes
// eviction O(sample) with no global priority queue and no lock beyond the shard's
// own. Redis's LRU approximation works the same way; LRB's contribution is the
// score function, not the search.
// Sample takes a callback and is reached through an interface, so the compiler has
// to assume the callback escapes. A closure built per Victim call would therefore
// allocate on every eviction, under the shard lock, precisely when the cache is
// under memory pressure. Building it once and keeping the running best in fields
// avoids that; it is safe because a shard's policy is only ever entered under that
// shard's lock, so there is never a second Victim in flight on the same object.
type sampled struct {
	score func(e *Entry, nowUS int64) float64
	visit func(*Entry) bool
	name  string

	bestScore float64
	nowUS     int64
	best      uint64
	n         int
	found     bool
}

// newSampled takes the candidate count rather than defaulting one, because the
// sample size is a policy decision and every constructor is reached from one place
// that reads CACHE_SAMPLE_SIZE. Five is Redis's default and is already within a
// couple of percent of exact LRU; the learned policy benefits from more, because a
// better score function deserves a wider search, which is why the default is 8.
func newSampled(name string, n int, score func(*Entry, int64) float64) *sampled {
	p := &sampled{name: name, n: n, score: score}
	p.visit = p.consider
	return p
}

func (p *sampled) Name() string { return p.name }

func (p *sampled) Victim(c Candidates, nowUS int64) (uint64, bool) {
	p.nowUS, p.bestScore, p.found = nowUS, math.Inf(-1), false
	c.Sample(p.n, p.visit)
	return p.best, p.found
}

func (p *sampled) consider(e *Entry) bool {
	if s := p.score(e, p.nowUS); s > p.bestScore {
		p.bestScore, p.best, p.found = s, e.key, true
	}
	return true
}

func (p *sampled) OnAccess(*Entry) {}
func (p *sampled) OnAdmit(*Entry)  {}
func (p *sampled) OnRemove(*Entry) {}

// NewLRU evicts the least recently used of the sampled candidates.
func NewLRU(sampleSize int) Policy {
	return newSampled("lru", sampleSize, func(e *Entry, nowUS int64) float64 {
		return float64(nowUS - e.lastAccess) // longest idle wins
	})
}

// NewLFU evicts the least frequently used of the sampled candidates. No aging,
// so a once-hot object can squat forever.
// ponytail: add a decay pass if this baseline ever needs to be competitive
// rather than illustrative.
func NewLFU(sampleSize int) Policy {
	return newSampled("lfu", sampleSize, func(e *Entry, _ int64) float64 {
		return -float64(e.accesses)
	})
}
