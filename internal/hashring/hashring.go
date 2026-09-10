// Package hashring implements consistent hashing with bounded loads.
//
// Plain consistent hashing gives you the property everyone wants: adding or
// removing a node moves only about 1/n of the keyspace. It does not give you any
// bound on how unequal the load can get. One viral key, or one unlucky hash
// distribution, and a single node takes a disproportionate share while its peers
// idle.
//
// Consistent Hashing with Bounded Loads (Mirrokni, Thorup, Zadimoghaddam, 2016,
// the algorithm behind Google's Cloud Load Balancing) fixes that with one extra
// rule: every node has a capacity of c times the current average load, and a key
// that lands on a full node walks forward around the ring to the next node with
// room. Keys stay sticky while there is slack, and spill predictably when there
// is not. The bound is what turns a hot key from an outage into a small amount of
// extra cache duplication.
package hashring

import (
	"errors"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/JustinK33/Belady/internal/cache"
)

var ErrEmpty = errors.New("hashring: no nodes")

type node struct {
	name string
	load atomic.Int64
}

type vnode struct {
	hash uint64
	idx  int32
}

type Ring struct {
	byName map[string]*node
	nodes  []*node
	vnodes []vnode // sorted by hash

	total atomic.Int64

	// factor is c from the paper. 1.25 means a node may carry 25% more than the
	// average before keys spill past it. Lower is more even and less sticky;
	// higher is stickier and more skewed.
	factor   float64
	replicas int

	// mu guards the topology only. Load accounting is atomic, so the read path
	// takes a read lock and never blocks another request.
	mu sync.RWMutex
}

func New(replicas int, factor float64) *Ring {
	if replicas <= 0 {
		replicas = 128
	}
	if factor <= 1 {
		factor = 1.25
	}
	return &Ring{
		byName:   make(map[string]*node),
		factor:   factor,
		replicas: replicas,
	}
}

func (r *Ring) Add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byName[name]; ok {
		return
	}
	n := &node{name: name}
	r.nodes = append(r.nodes, n)
	r.byName[name] = n

	idx := int32(len(r.nodes) - 1)
	for i := range r.replicas {
		r.vnodes = append(r.vnodes, vnode{hash: cache.Hash(name + "#" + strconv.Itoa(i)), idx: idx})
	}
	sort.Slice(r.vnodes, func(a, b int) bool { return r.vnodes[a].hash < r.vnodes[b].hash })
}

func (r *Ring) Members() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, n.name)
	}
	return out
}

// Pick returns the node that should serve key and charges it one unit of load.
// The caller must call Done with the returned name once the request finishes,
// which is what makes the bound track real concurrency rather than a request
// count.
func (r *Ring) Pick(key string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.nodes) == 0 {
		return "", ErrEmpty
	}
	if len(r.nodes) == 1 {
		r.nodes[0].load.Add(1)
		r.total.Add(1)
		return r.nodes[0].name, nil
	}

	capacity := r.capacityLocked()
	h := cache.Hash(key)

	// First virtual node at or after h, wrapping.
	start := sort.Search(len(r.vnodes), func(i int) bool { return r.vnodes[i].hash >= h })

	// Sum of capacities strictly exceeds total load, so some node has room and the
	// walk terminates. It is bounded by the ring size regardless, because a torn
	// read of load could otherwise loop.
	for i := range len(r.vnodes) {
		n := r.nodes[r.vnodes[(start+i)%len(r.vnodes)].idx]
		if n.load.Load() < capacity {
			n.load.Add(1)
			r.total.Add(1)
			return n.name, nil
		}
	}

	// Every node reads as full, which a concurrent burst can produce between the
	// load read and the increment. Fall back to the key's home node: overshooting
	// the bound slightly is better than failing the request.
	n := r.nodes[r.vnodes[start%len(r.vnodes)].idx]
	n.load.Add(1)
	r.total.Add(1)
	return n.name, nil
}

func (r *Ring) Done(name string) {
	r.mu.RLock()
	n, ok := r.byName[name]
	r.mu.RUnlock()

	if !ok {
		return
	}
	n.load.Add(-1)
	r.total.Add(-1)
}

// capacityLocked is ceil(((total+1)/n) * factor), the per-node ceiling from the
// paper. The +1 accounts for the request being placed, so a ring with zero load
// still admits one request per node.
func (r *Ring) capacityLocked() int64 {
	avg := float64(r.total.Load()+1) / float64(len(r.nodes))
	c := int64(avg*r.factor) + 1
	if c < 1 {
		c = 1
	}
	return c
}

// Loads reports in-flight load per node, for the gateway's metrics.
func (r *Ring) Loads() map[string]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]int64, len(r.nodes))
	for _, n := range r.nodes {
		out[n.name] = n.load.Load()
	}
	return out
}
