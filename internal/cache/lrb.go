package cache

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/JustinK33/Belady/internal/features"
	"github.com/JustinK33/Belady/internal/model"
)

// ModelHolder is the one place a trained model lives, shared by every shard.
//
// Shards each get their own policy object but all read the same holder, so a new
// model version becomes live everywhere with a single pointer store and no shard
// ever has to be locked to install it. The model itself is immutable once parsed,
// which is what makes that safe: an eviction already walking the old model keeps
// walking a tree nobody will mutate.
type ModelHolder struct {
	current atomic.Pointer[loaded]
}

type loaded struct {
	model    *model.Model
	version  string
	boundary time.Duration
}

func NewModelHolder() *ModelHolder { return &ModelHolder{} }

// Store validates and installs a model.
//
// The feature-count check is the whole reason this returns an error. A model trained
// against a different feature layout loads cleanly and evaluates without complaint;
// it just reads the wrong column for every split. Rejecting it here, once, is why
// Model.Raw can skip bounds validation per candidate.
func (h *ModelHolder) Store(m *model.Model, version string, boundary time.Duration) error {
	if m.NumFeatures() != features.Count {
		return fmt.Errorf("model %s expects %d features, this build produces %d (%v)",
			version, m.NumFeatures(), features.Count, features.Names)
	}
	if boundary <= 0 {
		return fmt.Errorf("model %s has a non-positive Belady boundary %v", version, boundary)
	}
	h.current.Store(&loaded{model: m, version: version, boundary: boundary})
	return nil
}

// Version reports the installed model, or "" when the cache is still running on its
// fallback policy. Surfaced in Stats so a hit-ratio comparison can never be
// misattributed to a model that was not actually loaded.
func (h *ModelHolder) Version() string {
	if l := h.current.Load(); l != nil {
		return l.version
	}
	return ""
}

func (h *ModelHolder) Boundary() time.Duration {
	if l := h.current.Load(); l != nil {
		return l.boundary
	}
	return 0
}

// lrb is Learned Relaxed Belady: score the sampled candidates with a gradient-boosted
// tree that was trained to answer "is this object's next access beyond the boundary",
// and evict the one most likely to be.
//
// Relaxed Belady is the trick that makes this trainable. Predicting when an object
// will next be requested is a hard regression problem and mostly wasted effort,
// because a cache does not need to know whether the next access is in ten minutes or
// ten hours: both are past the point where keeping the object pays. Collapsing that
// into one binary question turns it into a classification the model gets right often
// enough to matter. See docs/02-learned-eviction.md.
// Everything mutable here is per-shard state reused across the candidates of one
// eviction, for the same reason sampled keeps its running best in fields: the visitor
// closure would otherwise be allocated on every eviction.
type lrb struct {
	holder *ModelHolder

	// fallback runs until a model is installed. A fresh cluster has no model, and a
	// cache that refused to evict until one arrived would simply stop admitting.
	fallback Policy

	visit   func(*Entry) bool
	scratch []float32
	active  *model.Model

	in    features.Input
	worst uint64
	n     int
	best  float32
	found bool
}

// NewLRB returns a policy that evicts by learned score, falling back to LRU until a
// model is installed in holder.
func NewLRB(holder *ModelHolder, sampleSize int) Policy {
	if sampleSize <= 0 {
		sampleSize = 8
	}
	p := &lrb{
		holder:   holder,
		fallback: NewLRU(sampleSize),
		scratch:  make([]float32, features.Count),
		n:        sampleSize,
	}
	p.visit = p.consider
	return p
}

func (p *lrb) Name() string { return "lrb" }

func (p *lrb) Victim(c Candidates, nowUS int64) (uint64, bool) {
	l := p.holder.current.Load()
	if l == nil {
		return p.fallback.Victim(c, nowUS)
	}

	p.active, p.found = l.model, false
	p.in.NowUS = nowUS
	c.Sample(p.n, p.visit)
	return p.worst, p.found
}

func (p *lrb) consider(e *Entry) bool {
	in := &p.in
	in.Deltas = e.Deltas()
	in.LastAccessUS = e.LastAccess()
	in.AdmittedUS = e.Admitted()
	in.SizeBytes = e.Size()
	in.Accesses = e.Accesses()
	in.Frequency = e.Frequency()
	features.Extract(p.scratch, in)

	// Raw, not Score: the logistic is monotone, so it cannot change which candidate
	// ranks worst, and skipping it saves an Exp per candidate.
	if score := p.active.Raw(p.scratch); !p.found || score > p.best {
		p.worst, p.best, p.found = e.Key(), score, true
	}
	return true
}

// The learned policy needs no bookkeeping of its own: every feature is derived from
// the history Entry already maintains for all policies.
func (p *lrb) OnAccess(*Entry) {}
func (p *lrb) OnAdmit(*Entry)  {}
func (p *lrb) OnRemove(*Entry) {}
