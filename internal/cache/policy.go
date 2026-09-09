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
type sampled struct {
	score func(e *Entry, nowUS int64) float64
	name  string
	n     int
}

func (p *sampled) Name() string { return p.name }

func (p *sampled) Victim(c Candidates, nowUS int64) (uint64, bool) {
	best, bestScore, found := uint64(0), math.Inf(-1), false
	c.Sample(p.n, func(e *Entry) bool {
		if s := p.score(e, nowUS); s > bestScore {
			bestScore, best, found = s, e.key, true
		}
		return true
	})
	return best, found
}

func (p *sampled) OnAccess(*Entry) {}
func (p *sampled) OnAdmit(*Entry)  {}
func (p *sampled) OnRemove(*Entry) {}

// NewLRU evicts the least recently used of the sampled candidates.
func NewLRU(sampleSize int) Policy {
	return &sampled{
		name: "lru",
		n:    sampleSize,
		score: func(e *Entry, nowUS int64) float64 {
			return float64(nowUS - e.lastAccess) // longest idle wins
		},
	}
}

// NewLFU evicts the least frequently used of the sampled candidates. No aging,
// so a once-hot object can squat forever.
// ponytail: add a decay pass if this baseline ever needs to be competitive
// rather than illustrative.
func NewLFU(sampleSize int) Policy {
	return &sampled{
		name: "lfu",
		n:    sampleSize,
		score: func(e *Entry, _ int64) float64 {
			return -float64(e.accesses)
		},
	}
}
