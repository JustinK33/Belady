package cache

// HistoryLen is how many inter-access gaps each entry remembers. The LRB paper
// keeps 32; 8 holds per-entry history to 32 bytes and costs little accuracy on
// the workloads here. Raising it costs memory per cached object, not CPU.
const HistoryLen = 8

// Entry is one cached object plus the metadata every policy scores it on.
//
// Values are individually allocated and handed out without copying. An arena
// would cut GC pressure, but grpc-go's codec copies the value into the response
// message regardless, so an arena would buy allocator wins only, in exchange for
// a use-after-free class of bug on eviction.
// ponytail: per-object allocation; revisit only if pprof shows GC dominating.
type Entry struct {
	value []byte

	key        uint64
	lastAccess int64 // microseconds since the Unix epoch
	admitted   int64 // microseconds since the Unix epoch

	// deltas[0] is the gap before the most recent access, in milliseconds.
	// Milliseconds rather than the paper's seconds because a local cache sees
	// sub-second reuse constantly, and uint32 still spans 49 days.
	deltas [HistoryLen]uint32

	size     int32
	accesses uint32

	freq uint8 // saturating counter, read by s3fifo
}

func (e *Entry) Key() uint64           { return e.key }
func (e *Entry) Value() []byte         { return e.value }
func (e *Entry) Size() int32           { return e.size }
func (e *Entry) LastAccess() int64     { return e.lastAccess }
func (e *Entry) Admitted() int64       { return e.admitted }
func (e *Entry) Accesses() uint32      { return e.accesses }
func (e *Entry) Age(nowUS int64) int64 { return nowUS - e.admitted }

// Deltas returns a pointer so callers can read the history without copying 32
// bytes on a path that runs once per eviction candidate.
func (e *Entry) Deltas() *[HistoryLen]uint32 { return &e.deltas }

// touch folds a new access into the entry's history. Called with the shard lock
// held, on every hit, so it must not allocate.
func (e *Entry) touch(nowUS int64) {
	gap := nowUS - e.lastAccess
	if gap < 0 {
		gap = 0
	}
	ms := gap / 1000
	if ms > int64(^uint32(0)) {
		ms = int64(^uint32(0))
	}

	// Shift the history one slot older. A ring buffer would avoid the copy, but
	// 32 bytes is a single cache line and the index arithmetic would then be
	// paid back on every feature extraction instead.
	copy(e.deltas[1:], e.deltas[:HistoryLen-1])
	e.deltas[0] = uint32(ms)

	e.lastAccess = nowUS
	if e.accesses < ^uint32(0) {
		e.accesses++
	}
	if e.freq < 3 {
		e.freq++
	}
}
