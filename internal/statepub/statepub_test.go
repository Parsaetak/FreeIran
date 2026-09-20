// statepub_test.go pins the publisher contract:
//
//	duplicate suppression    — identical snapshot = no event
//	real change delivery     — every real change reaches the consumer
//	ordered burst delivery   — a fast burst of DISTINCT transitions is
//	                           delivered in order, none silently lost
//	zero artificial delay    — delivery begins without any timer window
//	synchronous stop         — no callback and no goroutine after Stop
//	stop drains the queue    — snapshots published before Stop are
//	                           delivered before termination
//	stop-then-publish safety — late Publish is a no-op, never a panic
//
// All tests are deterministic: the publisher itself contains no timer
// or sleep, so assertions only wait for observable delivery.
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

	p := New("test", equal)
	defer p.Stop()

	p.Subscribe(emit)

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

	p := New("test", equal)
	defer p.Stop()

	p.Subscribe(emit)

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

// TestFastBurstPreservesEveryDistinctTransition pins the §8 contract:
// a burst of DISTINCT lifecycle transitions (the
// selecting → preparing → starting_core → waiting_for_ready shape)
// must be delivered IN ORDER with none silently lost — there is no
// coalescing window that could replace pending snapshots with the
// newest one.
func TestFastBurstPreservesEveryDistinctTransition(t *testing.T) {
	emit, seen, mu, _ := collector(8)

	p := New("test", equal)
	defer p.Stop()

	p.Subscribe(emit)

	lifecycle := []string{
		"selecting", "preparing", "starting_core", "waiting_for_ready",
	}

	// A tight back-to-back burst: every distinct snapshot must be
	// queued and delivered in publication order.
	for i, status := range lifecycle {
		p.Publish(snapshot{status, int64(i)})
	}

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(*seen) == len(lifecycle)
	})

	mu.Lock()
	defer mu.Unlock()

	if len(*seen) != len(lifecycle) {
		t.Fatalf("burst delivered %d emissions, want %d — transitions were lost", len(*seen), len(lifecycle))
	}

	for i, status := range lifecycle {
		if (*seen)[i].status != status {
			t.Fatalf("emission %d = %q, want %q (order broken)", i, (*seen)[i].status, status)
		}
	}
}

// TestZeroDelayDelivery pins the §7 contract: delivery starts without
// any artificial timer window. A snapshot published to an idle
// publisher must reach the subscriber well inside any human-scale
// (let alone the old 25 ms) delay budget.
func TestZeroDelayDelivery(t *testing.T) {
	emit, _, _, notify := collector(2)

	p := New("test", equal)
	defer p.Stop()

	p.Subscribe(emit)

	start := time.Now()
	p.Publish(snapshot{"connected", 1})

	select {
	case <-*notify:
		elapsed := time.Since(start)
		if elapsed > 20*time.Millisecond {
			t.Fatalf("delivery took %v — an artificial delay is injecting latency", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot not delivered within 2s")
	}
}

// TestStopDrainsPendingSnapshots pins the shutdown contract: snapshots
// published before Stop (the terminal state in particular) are
// delivered BEFORE the publisher terminates — Stop never discards a
// queued transition.
func TestStopDrainsPendingSnapshots(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	var delivered []snapshot
	var mu sync.Mutex

	p := New("test", equal)

	p.Subscribe(func(s snapshot) {
		// Hold the FIRST delivery so later publishes queue up behind
		// it, then record everything.
		select {
		case <-started:
		default:
			close(started)
			<-release
		}

		mu.Lock()
		delivered = append(delivered, s)
		mu.Unlock()
	})

	p.Publish(snapshot{"preparing", 1})

	<-started // the delivery goroutine is inside the first callback

	p.Publish(snapshot{"starting_core", 2})
	p.Publish(snapshot{"waiting_for_ready", 3})
	p.Publish(snapshot{"connected", 4})

	// Stop while the queue holds three pending snapshots; Stop must
	// block on the in-flight callback, then deliver all three before
	// returning.
	stopped := make(chan struct{})

	go func() {
		p.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while a callback was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the callback finished")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(delivered) != 4 {
		t.Fatalf("Stop drained %d snapshots, want 4 — pending transitions were dropped", len(delivered))
	}

	if delivered[3].status != "connected" {
		t.Fatalf("final delivered snapshot = %q, want connected", delivered[3].status)
	}
}

func TestStopIsSynchronousAndTerminal(t *testing.T) {
	var emitting atomic.Bool

	release := make(chan struct{})

	p := New("test", equal)

	p.Subscribe(func(snapshot) {
		emitting.Store(true)

		<-release // block inside the callback to prove Stop joins
	})

	p.Publish(snapshot{"ready", 1})

	// Wait until the callback is actually running.
	waitFor(t, 2*time.Second, emitting.Load)

	stopped := make(chan struct{})

	go func() {
		p.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while the callback was still running")
	case <-time.After(50 * time.Millisecond):
		// expected: Stop is joined on the delivery goroutine
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

	p := New("test", equal)
	p.Subscribe(emit)
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

	p := New("test", equal)

	p.Subscribe(func(snapshot) { callbacks.Add(1) })

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

// TestSubscriberCancelAndMultipleSubscribers pins the subscription
// surface: several subscribers all receive deliveries; a cancelled
// subscriber receives nothing further while the others keep working.
func TestSubscriberCancelAndMultipleSubscribers(t *testing.T) {
	p := New("test", equal)
	defer p.Stop()

	var mu sync.Mutex

	var a, b []snapshot

	cancelA := p.Subscribe(func(s snapshot) {
		mu.Lock()
		a = append(a, s)
		mu.Unlock()
	})

	_ = p.Subscribe(func(s snapshot) {
		mu.Lock()
		b = append(b, s)
		mu.Unlock()
	})

	p.Publish(snapshot{"one", 1})

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(a) == 1 && len(b) == 1
	})

	cancelA()

	p.Publish(snapshot{"two", 2})

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(b) == 2
	})

	mu.Lock()
	defer mu.Unlock()

	if len(a) != 1 {
		t.Fatalf("cancelled subscriber received %d deliveries, want 1", len(a))
	}

	if len(b) != 2 {
		t.Fatalf("active subscriber received %d deliveries, want 2", len(b))
	}
}

// TestOverflowValveKeepsNewestAndOrder pins the bounded-memory
// contract: when the pending queue overflows (a stalled consumer with
// more than maxQueue outstanding transitions), the OLDEST pending
// snapshot is dropped to admit the newest — delivery order is
// preserved and the queue never grows without bound.
func TestOverflowValveKeepsNewestAndOrder(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	var mu sync.Mutex

	var delivered []int64

	p := New("test", equal)

	p.Subscribe(func(s snapshot) {
		select {
		case <-started:
		default:
			close(started)
			<-release // stall the consumer
		}

		mu.Lock()
		delivered = append(delivered, s.rev)
		mu.Unlock()
	})

	p.Publish(snapshot{"held", -1})

	<-started // delivery goroutine is stalled inside the callback

	// Overfill the queue past its bound.
	for i := 0; i < maxQueue+50; i++ {
		p.Publish(snapshot{"bulk", int64(i)})
	}

	close(release)

	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(delivered) == maxQueue+1
	})

	p.Stop()

	mu.Lock()
	defer mu.Unlock()

	// rev -1 (held) was delivered first; of the bulk burst, the oldest
	// 50 were dropped by the valve; the rest arrive in order, newest
	// last.
	if len(delivered) != maxQueue+1 {
		t.Fatalf("delivered %d snapshots, want %d", len(delivered), maxQueue+1)
	}

	for i := 1; i < len(delivered); i++ {
		if delivered[i] <= delivered[i-1] {
			t.Fatalf("order broken at %d: %v", i, delivered)
		}
	}

	if got := delivered[len(delivered)-1]; got != int64(maxQueue+49) {
		t.Fatalf("newest snapshot %d lost, last delivered rev = %d", int64(maxQueue+49), got)
	}
}
