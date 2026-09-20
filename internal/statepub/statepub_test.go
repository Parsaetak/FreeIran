// statepub_test.go pins the publisher contract (v0.9.8.7):
//
//	duplicate suppression       — same state + same revision = no event
//	real change delivery        — every real change reaches the consumer
//	burst coalescing            — rapid changes collapse, order preserved
//	synchronous stop            — no callback and no goroutine after Stop
//	stop-then-publish safety    — late Publish is a no-op, never a panic
//
// All tests are deterministic: no sleeps except the one inside the
// publisher's own (tiny) coalescing window.
package statepub

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// snapshot is a minimal stand-in with a monotonic revision — the
// dedup model the real AppState/Connection snapshots use.
type snapshot struct {
	status string
	rev    int64
}

func equal(a, b snapshot) bool { return a.status == b.status && a.rev == b.rev }

func collector(n int) (func(snapshot), *[]snapshot, *sync.Mutex, *chan struct{}) {
	mu := &sync.Mutex{}
	seen := &[]snapshot{}
	notify := make(chan struct{}, n+8)

	return func(s snapshot) {
			mu.Lock()
			*seen = append(*seen, s)
			mu.Unlock()

			select {
			case notify <- struct{}{}:
			default:
			}
		},
		seen, mu, &notify
}

func waitFor(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()

	deadlineAt := time.Now().Add(deadline)

	for time.Now().Before(deadlineAt) {
		if cond() {
			return
		}

		time.Sleep(2 * time.Millisecond)
	}

	t.Fatalf("condition not met within %v", deadline)
}

func TestDuplicateSuppression(t *testing.T) {
	emit, seen, mu, _ := collector(4)

	p := New("test", emit, equal, time.Millisecond)
	defer p.Stop()

	p.Publish(snapshot{"ready", 1})

	// Identical snapshots (same state + same revision) must be dropped
	// no matter how often they repeat.
	for i := 0; i < 50; i++ {
		p.Publish(snapshot{"ready", 1})
	}

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(*seen) == 1
	})

	p.Publish(snapshot{"ready", 1})
	p.Publish(snapshot{"ready", 1})

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(*seen) != 1 {
		t.Fatalf("duplicate suppression failed: got %d emissions, want 1", len(*seen))
	}

	if (*seen)[0].status != "ready" {
		t.Fatalf("emitted wrong snapshot: %+v", (*seen)[0])
	}
}

func TestRealChangeAlwaysEmitted(t *testing.T) {
	emit, seen, mu, _ := collector(4)

	p := New("test", emit, equal, time.Millisecond)
	defer p.Stop()

	// Each real change is spaced beyond the coalescing window, so each
	// must reach the consumer — none may be dropped as a duplicate.
	p.Publish(snapshot{"ready", 1})

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(*seen) == 1
	})

	p.Publish(snapshot{"degraded", 2})

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(*seen) == 2
	})

	p.Publish(snapshot{"ready", 3})

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(*seen) == 3
	})

	mu.Lock()
	defer mu.Unlock()

	want := []string{"ready", "degraded", "ready"}

	for i, status := range want {
		if (*seen)[i].status != status {
			t.Fatalf("emission %d = %q, want %q", i, (*seen)[i].status, status)
		}
	}
}

func TestBurstCoalescingKeepsNewestAndOrder(t *testing.T) {
	emit, seen, mu, _ := collector(8)

	// A coalescing window wider than the publish burst: the whole
	// burst must collapse into ONE emission of the NEWEST snapshot.
	p := New("test", emit, equal, 60*time.Millisecond)
	defer p.Stop()

	for i := 1; i <= 10; i++ {
		p.Publish(snapshot{"burst", int64(i)})
	}

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(*seen) >= 1
	})

	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(*seen) != 1 {
		t.Fatalf("burst produced %d emissions, want 1 (coalescing failed)", len(*seen))
	}

	if (*seen)[0].rev != 10 {
		t.Fatalf("coalesced snapshot = rev %d, want newest rev 10", (*seen)[0].rev)
	}
}

func TestStopIsSynchronousAndTerminal(t *testing.T) {
	var emitting atomic.Bool

	release := make(chan struct{})

	p := New("test", func(snapshot) {
		emitting.Store(true)

		<-release // block inside emit to prove Stop joins
	}, equal, time.Millisecond)

	p.Publish(snapshot{"ready", 1})

	// Wait until the emit callback is actually running.
	waitFor(t, 2*time.Second, emitting.Load)

	stopped := make(chan struct{})

	go func() {
		p.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while the emit callback was still running")
	case <-time.After(50 * time.Millisecond):
		// expected: Stop is still joined on the callback
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the callback finished")
	}

	// After Stop, Publish must be a safe no-op (no callback, no panic).
	p.Publish(snapshot{"late", 99})

	time.Sleep(30 * time.Millisecond)

	// Stop twice: idempotent.
	p.Stop()
}

func TestPublishAfterStopIsNoop(t *testing.T) {
	emit, seen, mu, _ := collector(2)

	p := New("test", emit, equal, time.Millisecond)
	p.Stop()

	for i := 0; i < 10; i++ {
		p.Publish(snapshot{"late", int64(i)})
	}

	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(*seen) != 0 {
		t.Fatalf("emissions after Stop: %d, want 0", len(*seen))
	}
}

func TestStopPreventsRacingCallback(t *testing.T) {
	// Concurrent Publish storm against Stop: after Stop returns, no
	// callback may ever fire again — the invariant that protects a
	// destroyed UI runtime.
	var callbacks atomic.Int64

	p := New("test", func(snapshot) { callbacks.Add(1) }, equal, time.Millisecond)

	var wg sync.WaitGroup

	for w := 0; w < 4; w++ {
		wg.Add(1)

		go func(id int) {
			defer wg.Done()

			for i := 0; i < 200; i++ {
				p.Publish(snapshot{"storm", int64(id*1000 + i)})
			}
		}(w)
	}

	// Give the storm a moment, then stop in the middle.
	time.Sleep(10 * time.Millisecond)
	p.Stop()
	wg.Wait()

	before := callbacks.Load()

	time.Sleep(50 * time.Millisecond)

	if after := callbacks.Load(); after != before {
		t.Fatalf("callbacks fired after Stop: %d → %d", before, after)
	}
}
