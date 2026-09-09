// Package belady computes the offline optimum the whole project is named after.
//
// Belady's MIN (Belady, 1966) evicts the object whose next request is furthest in
// the future. It is provably optimal and completely unimplementable online,
// because it requires the future. Given a recorded trace the future is available,
// so MIN can be computed after the fact and used as the ceiling every online
// policy is measured against. Without it, "0.74 hit ratio" means nothing: it could
// be two points off optimal or thirty.
package belady

import "container/heap"

// MINHitRatio returns the object hit ratio of the optimal policy over trace, for
// a cache holding capacity objects.
//
// Capacity is in objects, not bytes, and that is a real limitation. MIN is only
// provably optimal when every object is the same size; with variable sizes the
// offline optimum becomes an NP-hard knapsack-over-time problem, usually
// approximated with a min-cost flow. Treat the result as exact for a uniform-size
// workload and as a close estimate otherwise.
// ponytail: count-based MIN. A flow-based bound is the upgrade if variable-size
// workloads ever need a rigorous ceiling.
func MINHitRatio(trace []uint64, capacity int) float64 {
	return MINHitRatioFrom(trace, capacity, 0)
}

// MINHitRatioFrom simulates the whole trace but scores only requests at index
// from onward.
//
// This exists because comparing against a warm cache requires it. A server that
// replayed a warmup before the measured window starts the measurement with a
// populated cache, so scoring MIN from cold compares a warm policy against a cold
// optimum, and the "optimum" can come out lower than the policy it is supposed to
// bound. Feed the warmup as a prefix and set from to its length.
func MINHitRatioFrom(trace []uint64, capacity, from int) float64 {
	if capacity <= 0 || len(trace) == 0 || from >= len(trace) {
		return 0
	}
	if from < 0 {
		from = 0
	}

	// next[i] is the index of the following request for the same object, or
	// len(trace) if there is none. Built by walking the trace backwards, which is
	// the only reason this is O(n) rather than O(n^2).
	next := make([]int, len(trace))
	seen := make(map[uint64]int, len(trace)/4+1)
	for i := len(trace) - 1; i >= 0; i-- {
		if j, ok := seen[trace[i]]; ok {
			next[i] = j
		} else {
			next[i] = len(trace)
		}
		seen[trace[i]] = i
	}

	// resident maps a cached key to the next-use index it is currently keyed on.
	// The heap may hold older entries for the same key; those are stale and skipped
	// on pop, which is cheaper than finding and updating them in place.
	resident := make(map[uint64]int, capacity+1)
	h := make(furthestFirst, 0, capacity+1)

	hits := 0
	for i, key := range trace {
		_, cached := resident[key]
		if cached {
			if i >= from {
				hits++
			}
		} else if len(resident) >= capacity {
			evictFurthest(&h, resident)
		}
		resident[key] = next[i]
		heap.Push(&h, entry{nextUse: next[i], key: key})
	}
	return float64(hits) / float64(len(trace)-from)
}

func evictFurthest(h *furthestFirst, resident map[uint64]int) {
	for h.Len() > 0 {
		e := heap.Pop(h).(entry)
		if cur, ok := resident[e.key]; ok && cur == e.nextUse {
			delete(resident, e.key)
			return
		}
	}
}

type entry struct {
	key     uint64
	nextUse int
}

// furthestFirst is a max-heap on nextUse: the object needed furthest away, or
// never again, comes off first.
type furthestFirst []entry

func (h furthestFirst) Len() int           { return len(h) }
func (h furthestFirst) Less(i, j int) bool { return h[i].nextUse > h[j].nextUse }
func (h furthestFirst) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *furthestFirst) Push(x any)        { *h = append(*h, x.(entry)) }
func (h *furthestFirst) Pop() any {
	old := *h
	n := len(old) - 1
	e := old[n]
	*h = old[:n]
	return e
}
