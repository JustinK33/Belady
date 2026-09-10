package cache

import (
	"testing"
	"time"
)

// The fake clock advances 1ms per read, so a TTL of a few milliseconds expires after
// a handful of operations without anything sleeping.

func TestPutWithTTLExpires(t *testing.T) {
	c := newTestCache(t, 1<<20, 1, func(int64) Policy { return NewLRU(5) })

	if !c.Put("k", []byte("v"), 3*time.Millisecond) {
		t.Fatal("Put was refused")
	}
	if _, ok := c.Get("k"); !ok {
		t.Fatal("the entry expired before its TTL elapsed")
	}

	// Each Get advances the clock 1ms, so this is the deadline passing rather than a
	// fixed number of reads being magic.
	for range 5 {
		if _, ok := c.Get("k"); !ok {
			st := c.Snapshot()
			if st.Expirations != 1 {
				t.Errorf("Expirations = %d, want 1", st.Expirations)
			}
			if st.Objects != 0 || st.BytesUsed != 0 {
				t.Errorf("expired entry left %d objects and %d bytes behind", st.Objects, st.BytesUsed)
			}
			return
		}
	}
	t.Fatal("the entry never expired")
}

func TestPutWithoutTTLNeverExpires(t *testing.T) {
	c := newTestCache(t, 1<<20, 1, func(int64) Policy { return NewLRU(5) })

	c.Put("k", []byte("v"), 0)
	for range 100 {
		if _, ok := c.Get("k"); !ok {
			t.Fatal("an entry stored with no TTL expired")
		}
	}
	if got := c.Snapshot().Expirations; got != 0 {
		t.Errorf("Expirations = %d with no TTL configured", got)
	}
}

// TestDefaultTTLAppliesToAdmit is the read-through case: objects fetched from origin
// carry no TTL of their own, so CACHE_DEFAULT_TTL is the only thing that bounds how
// stale a cached response can get.
func TestDefaultTTLAppliesToAdmit(t *testing.T) {
	clk := &fakeClock{}
	c, err := New(Config{
		NewPolicy:     func(int64) Policy { return NewLRU(5) },
		NowUS:         clk.nowUS,
		Nanos:         func() int64 { return 0 },
		CapacityBytes: 1 << 20,
		Shards:        1,
		DefaultTTL:    3 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Admit("k", []byte("v"))
	expired := false
	for range 10 {
		if _, ok := c.Get("k"); !ok {
			expired = true
			break
		}
	}
	if !expired {
		t.Fatal("DefaultTTL did not apply to an admitted object")
	}
}

// TestExplicitTTLOverridesDefault matters because the two are combined per call, and
// getting the precedence backwards would silently cap every TTL at the default.
func TestExplicitTTLOverridesDefault(t *testing.T) {
	clk := &fakeClock{}
	c, err := New(Config{
		NewPolicy:     func(int64) Policy { return NewLRU(5) },
		NowUS:         clk.nowUS,
		Nanos:         func() int64 { return 0 },
		CapacityBytes: 1 << 20,
		Shards:        1,
		DefaultTTL:    2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Put("k", []byte("v"), time.Hour)
	for range 50 {
		if _, ok := c.Get("k"); !ok {
			t.Fatal("an explicit one-hour TTL was overridden by the 2ms default")
		}
	}
}
