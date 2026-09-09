// Package cache is the cache node's data plane: a sharded store plus the
// eviction policies that compete inside it.
//
// The whole point of the project lives in Policy. Everything else exists to make
// the policies comparable under identical conditions, and to keep the request
// path fast enough that the policy's own cost is visible.
package cache

import (
	"errors"
	"fmt"
	"math/bits"
	"time"
)

type Config struct {
	// NewPolicy is called once per shard, so a policy may keep unsynchronised
	// state.
	NewPolicy func() Policy

	// NowUS and Nanos are injectable so tests can drive time without sleeping.
	NowUS func() int64
	Nanos func() int64

	CapacityBytes int64
	// Shards is rounded up to a power of two so shard selection is a mask.
	Shards int
	// SampleSize is how many candidates a sampled policy scores per eviction.
	// Five is Redis's default and is already within a couple of percent of exact
	// LRU; the learned policy benefits from more, because a better score function
	// deserves a wider search.
	SampleSize int
}

type Cache struct {
	nowUS  func() int64
	policy string
	shards []*shard
	mask   uint64
}

// Stats is a point-in-time sum across shards. Object hit ratio and byte hit ratio
// diverge as soon as sizes are skewed: a cache can hold most requests while
// serving a minority of bytes, or the reverse, and which one matters depends on
// whether the bottleneck downstream is queries or bandwidth.
type Stats struct {
	Policy string

	Hits, Misses        uint64
	HitBytes, MissBytes uint64

	Admissions, Rejections, Evictions uint64

	Objects       uint64
	BytesUsed     uint64
	BytesCapacity uint64

	// EvictNSMean is the average time the policy spends choosing a victim. It is
	// the learned policy's budget line: a hit-ratio gain that costs milliseconds
	// per eviction is not a gain.
	EvictNSMean uint64
}

func (s Stats) ObjectHitRatio() float64 { return ratio(s.Hits, s.Hits+s.Misses) }
func (s Stats) ByteHitRatio() float64   { return ratio(s.HitBytes, s.HitBytes+s.MissBytes) }

func ratio(num, den uint64) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func (c *Cache) Snapshot() Stats {
	st := Stats{Policy: c.policy}
	var evictNS, evictOps uint64
	for _, s := range c.shards {
		s.mu.Lock()
		st.Hits += s.hits
		st.Misses += s.misses
		st.HitBytes += s.hitBytes
		st.MissBytes += s.missBytes
		st.Admissions += s.admissions
		st.Rejections += s.rejects
		st.Evictions += s.evictions
		st.Objects += uint64(len(s.m))
		st.BytesUsed += uint64(s.used)
		st.BytesCapacity += uint64(s.capacity)
		evictNS += s.evictNS
		evictOps += s.evictSample
		s.mu.Unlock()
	}
	if evictOps > 0 {
		st.EvictNSMean = evictNS / evictOps
	}
	return st
}

func New(cfg Config) (*Cache, error) {
	if cfg.CapacityBytes <= 0 {
		return nil, errors.New("cache: capacity must be positive")
	}
	if cfg.NewPolicy == nil {
		return nil, errors.New("cache: NewPolicy is required")
	}
	if cfg.Shards <= 0 {
		cfg.Shards = 256
	}
	if cfg.SampleSize <= 0 {
		cfg.SampleSize = 5
	}
	if cfg.NowUS == nil {
		cfg.NowUS = func() int64 { return time.Now().UnixMicro() }
	}
	if cfg.Nanos == nil {
		cfg.Nanos = func() int64 { return time.Now().UnixNano() }
	}

	n := 1 << bits.Len(uint(cfg.Shards-1))
	per := cfg.CapacityBytes / int64(n)
	if per <= 0 {
		return nil, fmt.Errorf("cache: %d bytes across %d shards leaves nothing per shard", cfg.CapacityBytes, n)
	}

	c := &Cache{
		nowUS:  cfg.NowUS,
		shards: make([]*shard, n),
		mask:   uint64(n - 1),
		policy: cfg.NewPolicy().Name(),
	}
	for i := range c.shards {
		c.shards[i] = newShard(per, cfg.SampleSize, cfg.NewPolicy(), cfg.Nanos)
	}
	return c, nil
}

func (c *Cache) shardFor(h uint64) *shard { return c.shards[h&c.mask] }

func (c *Cache) Get(key string) ([]byte, bool) {
	h := Hash(key)
	return c.shardFor(h).get(h, c.nowUS())
}

// Admit stores an object fetched from origin after a miss. The returned bool
// reports whether it was cached; a false does not mean the request failed.
func (c *Cache) Admit(key string, value []byte) bool {
	h := Hash(key)
	return c.shardFor(h).insert(h, value, c.nowUS(), true)
}

// Put stores an object on behalf of a client. Unlike Admit it is not counted as
// a miss, so it does not distort hit ratio.
func (c *Cache) Put(key string, value []byte) bool {
	h := Hash(key)
	return c.shardFor(h).insert(h, value, c.nowUS(), false)
}

func (c *Cache) Delete(key string) bool {
	h := Hash(key)
	return c.shardFor(h).remove(h)
}

func (c *Cache) Policy() string { return c.policy }

// Hash is FNV-1a followed by a splitmix64 finalizer. It has to be stable across
// processes, because the gateway picks a node from it, the shard index comes from
// its low bits, and it is the object's identity in the access trace. FNV alone
// avalanches poorly in the low bits, which is exactly where the shard mask looks.
func Hash(key string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= prime64
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}
