package mempressure

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestStateString(t *testing.T) {
	cases := map[State]string{
		StateNormal:   "normal",
		StateElevated: "elevated",
		StateHigh:     "high",
		StateCritical: "critical",
		State(99):     "unknown",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestDefaultCeiling(t *testing.T) {
	c := DefaultCeiling()
	if c.HeapBytes == 0 || c.RSSBytes == 0 || c.ArenaBytes == 0 {
		t.Errorf("DefaultCeiling has zero fields: %+v", c)
	}
}

func TestControllerHysteresis(t *testing.T) {
	// Use a very large heap/RSS ceiling so the real process heap
	// doesn't dominate the pressure signal. We drive pressure via
	// the cache consumer only.
	c := New(Ceiling{
		HeapBytes:  1 << 40, // 1 TiB — effectively unbounded
		RSSBytes:   1 << 40,
		GCPct:      100,
		ArenaBytes: 1 << 40,
		CacheBytes: 10000,
		QueueBytes: 1 << 40,
	})

	var stateChanges atomic.Int32
	c.SetListener(func(old, new State, snap Snapshot) {
		stateChanges.Add(1)
	})

	// Normal: cache=0.
	c.SetCacheBytes(0)
	snap := c.Sample()
	if snap.State != StateNormal {
		t.Errorf("initial state = %s, want normal", snap.State)
	}

	// Drive to elevated via cache: 70% > 0.60 up threshold.
	c.SetCacheBytes(7000)
	snap = c.Sample()
	if snap.State != StateElevated {
		t.Errorf("after cache=7000, state = %s, want elevated", snap.State)
	}

	// Drop to 4000 (40%). The down threshold for Elevated is 0.45, so
	// 40% should transition back to Normal.
	c.SetCacheBytes(4000)
	snap = c.Sample()
	if snap.State != StateNormal {
		t.Errorf("after cache=4000, state = %s, want normal (hysteresis)", snap.State)
	}

	// Drive all the way up.
	c.SetCacheBytes(8000) // 80% → High
	snap = c.Sample()
	if snap.State != StateHigh {
		t.Errorf("after cache=8000, state = %s, want high", snap.State)
	}
	c.SetCacheBytes(9500) // 95% → Critical
	snap = c.Sample()
	if snap.State != StateCritical {
		t.Errorf("after cache=9500, state = %s, want critical", snap.State)
	}

	// Drop back gradually (hysteresis).
	c.SetCacheBytes(8000) // 80%: Critical down threshold is 0.72, so still Critical
	snap = c.Sample()
	if snap.State != StateCritical {
		t.Errorf("after cache=8000 from critical, state = %s, want critical (above down threshold)", snap.State)
	}
	c.SetCacheBytes(7000) // 70%: below 0.72 → High
	snap = c.Sample()
	if snap.State != StateHigh {
		t.Errorf("after cache=7000, state = %s, want high", snap.State)
	}
	c.SetCacheBytes(5500) // 55%: below 0.60 (High down) → Elevated
	snap = c.Sample()
	if snap.State != StateElevated {
		t.Errorf("after cache=5500, state = %s, want elevated", snap.State)
	}
	c.SetCacheBytes(4000) // 40%: below 0.45 (Elevated down) → Normal
	snap = c.Sample()
	if snap.State != StateNormal {
		t.Errorf("after cache=4000, state = %s, want normal", snap.State)
	}

	if stateChanges.Load() < 6 {
		t.Errorf("state changes = %d, want at least 6", stateChanges.Load())
	}
}

func TestControllerSetCeiling(t *testing.T) {
	c := New(Ceiling{
		HeapBytes:  1 << 40,
		RSSBytes:   1 << 40,
		GCPct:      100,
		ArenaBytes: 1 << 40,
		CacheBytes: 1000,
		QueueBytes: 1 << 40,
	})
	// 65% → Elevated (above 0.60, below 0.75).
	c.SetCacheBytes(650)
	snap := c.Sample()
	if snap.State != StateElevated {
		t.Errorf("state = %s, want elevated (65%%)", snap.State)
	}
	// Raise the ceiling so 650 is now 32.5% → Normal.
	c.SetCeiling(Ceiling{
		HeapBytes:  1 << 40,
		RSSBytes:   1 << 40,
		GCPct:      100,
		ArenaBytes: 1 << 40,
		CacheBytes: 2000,
		QueueBytes: 1 << 40,
	})
	snap = c.Sample()
	if snap.State != StateNormal {
		t.Errorf("after ceiling raise, state = %s, want normal (32.5%%)", snap.State)
	}
}

func TestControllerConcurrentSet(t *testing.T) {
	c := New(DefaultCeiling())
	done := make(chan struct{})
	go func() {
		for i := uint64(0); i < 10000; i++ {
			c.SetArenaBytes(i)
			c.SetCacheBytes(i * 2)
			c.SetQueueBytes(i * 3)
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		c.Sample()
		time.Sleep(time.Microsecond)
	}
	<-done
}
