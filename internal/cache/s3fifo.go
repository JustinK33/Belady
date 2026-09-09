package cache

// S3-FIFO (Yang, Qiu, Zhao, Cheng, Zhang; SOSP 2023) is the interesting
// non-learned baseline. It beats LRU on most real workloads using nothing but
// three FIFO queues and a two-bit counter, which makes it the honest bar the
// learned policy has to clear: if a model cannot beat two bits of state, the
// model is not worth its training pipeline.
//
// The idea rests on the observation that most objects in a web cache are
// one-hit wonders. New objects land in a small probationary queue S. An object
// that is never re-requested leaves from S quickly and cheaply, having never
// polluted the main queue M. An object requested again while in S is promoted to
// M. G is a ghost queue holding only the keys of objects evicted from S, so a
// returning one-hit wonder can be recognised and sent straight to M.
//
// Queues here hold key hashes rather than intrusive list pointers. That costs a
// map lookup per queue step, and saves 16 bytes on every cached object plus a
// whole class of dangling-pointer bug.
type s3fifo struct {
	inS map[uint64]struct{}
	inG map[uint64]struct{}
	// Sliding-window slices: popping advances the header and append reallocates
	// once capacity runs out, so live memory stays within a small factor of the
	// queue length.
	// ponytail: a ring buffer would be tighter; not worth it until a heap profile
	// says these slices matter.
	small []uint64
	main  []uint64
	ghost []uint64

	smallBytes  int64
	smallTarget int64
	ghostTarget int
}

// smallFraction is the share of capacity given to the probationary queue. The
// paper's 10% holds up across their traces and is not worth tuning until the
// workload here says otherwise.
const smallFraction = 10

func NewS3FIFO(shardCapacityBytes int64) Policy {
	target := shardCapacityBytes / smallFraction
	if target <= 0 {
		target = 1
	}
	// The ghost queue remembers roughly as many keys as the main queue could hold.
	// Estimating that needs a mean object size, which is unknown up front, so
	// assume 1 KiB and clamp. Getting this wrong costs hit ratio, not correctness.
	ghost := int(shardCapacityBytes / 1024)
	if ghost < 16 {
		ghost = 16
	}
	return &s3fifo{
		inS:         make(map[uint64]struct{}),
		inG:         make(map[uint64]struct{}),
		smallTarget: target,
		ghostTarget: ghost,
	}
}

func (p *s3fifo) Name() string { return "s3fifo" }

// OnAccess does nothing: Entry.touch already saturates freq at 3, which is the
// counter S3-FIFO wants.
func (p *s3fifo) OnAccess(*Entry) {}

func (p *s3fifo) OnAdmit(e *Entry) {
	if _, seen := p.inG[e.key]; seen {
		// Seen before and requested again: skip probation entirely.
		delete(p.inG, e.key)
		p.main = append(p.main, e.key)
		e.freq = 0
		return
	}
	p.small = append(p.small, e.key)
	p.inS[e.key] = struct{}{}
	p.smallBytes += int64(e.size)
	e.freq = 0
}

func (p *s3fifo) OnRemove(e *Entry) {
	if _, ok := p.inS[e.key]; ok {
		delete(p.inS, e.key)
		p.smallBytes -= int64(e.size)
	}
}

// Victim walks the queues until it finds an object to drop. Promotions and
// reinsertions are pure queue moves, so the shard is untouched until a real
// victim is named.
//
// The loop is bounded by the total number of queued keys so a corrupt queue can
// never spin forever; each iteration either returns or removes one queue element.
func (p *s3fifo) Victim(c Candidates, _ int64) (uint64, bool) {
	// freq saturates at 3, so four laps of M drive every counter to zero and a
	// victim is guaranteed. The bound exists to make a corrupt queue terminate,
	// not to cut the search short.
	for steps := 4*(len(p.small)+len(p.main)) + 8; steps > 0; steps-- {
		if p.smallBytes >= p.smallTarget && len(p.small) > 0 {
			if key, evict := p.stepSmall(c); evict {
				return key, true
			}
			continue
		}
		if len(p.main) == 0 {
			// Everything lives in S and S is under target, which happens when the
			// shard holds a handful of large objects. Fall back to draining S.
			if len(p.small) == 0 {
				return 0, false
			}
			if key, evict := p.stepSmall(c); evict {
				return key, true
			}
			continue
		}
		if key, evict := p.stepMain(c); evict {
			return key, true
		}
	}
	return 0, false
}

func (p *s3fifo) stepSmall(c Candidates) (uint64, bool) {
	key := p.small[0]
	p.small = p.small[1:]

	e, live := c.Lookup(key)
	if !live {
		// Already deleted by a client; OnRemove cleaned up the accounting.
		return 0, false
	}
	delete(p.inS, key)
	p.smallBytes -= int64(e.size)

	if e.freq > 0 {
		// Requested again during probation: promote rather than evict.
		p.main = append(p.main, key)
		e.freq = 0
		return 0, false
	}

	p.rememberGhost(key)
	return key, true
}

func (p *s3fifo) stepMain(c Candidates) (uint64, bool) {
	key := p.main[0]
	p.main = p.main[1:]

	e, live := c.Lookup(key)
	if !live {
		return 0, false
	}
	if e.freq > 0 {
		// One more lap, with a smaller claim on survival. This is CLOCK's
		// second-chance rule, applied to a FIFO.
		e.freq--
		p.main = append(p.main, key)
		return 0, false
	}
	return key, true
}

func (p *s3fifo) rememberGhost(key uint64) {
	p.ghost = append(p.ghost, key)
	p.inG[key] = struct{}{}
	for len(p.ghost) > p.ghostTarget {
		delete(p.inG, p.ghost[0])
		p.ghost = p.ghost[1:]
	}
}
