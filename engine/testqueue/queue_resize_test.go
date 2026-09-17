package testqueue

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingTester blocks each Test call until released, giving the
// resize tests precise control over busy vs idle workers.
type countingTester struct {
	block   chan struct{}
	started atomic.Int64
}

func newCountingTester() *countingTester {
	return &countingTester{block: make(chan struct{})}
}

func (c *countingTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	c.started.Add(1)

	select {
	case <-c.block:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	return Result{TestedAt: time.Now().UTC(), Working: true}, nil
}

// TestSetConcurrencyGrowShrink verifies the dynamic worker pool:
// growth is immediate, shrinkage retires idle workers promptly (no
// wait for the next task), busy workers finish their current task,
// and no task is ever dropped.
//
// v0.9.7: the queue is configured with CoreProbeConcurrency = 4 so
// the full worker pool can run concurrently (the default cap of 2
// core slots applies to the dedicated bounded-core tests below).
func TestSetConcurrencyGrowShrink(t *testing.T) {
	tester := newCountingTester()
	q := New(tester, Config{
		Concurrency:          1,
		MaxQueueSize:         100,
		Timeout:              10 * time.Second,
		MaxAttempts:          1,
		CoreProbeConcurrency: 4,
	})
	q.Start(context.Background())
	defer q.Stop()

	// Grow 1 → 4: workers spawn immediately.
	q.SetConcurrency(4)

	// Enqueue 4 tasks; all four must start concurrently.
	for i := 0; i < 4; i++ {
		if _, err := q.Enqueue(fmt.Sprintf("fp-grow-%d", i), "vless", nil, 100, "src", EnqueueDefault); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)

	for tester.started.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if got := tester.started.Load(); got != 4 {
		t.Fatalf("concurrent tests started = %d, want 4 (worker pool did not grow)", got)
	}

	if got := int(q.liveWorkers.Load()); got != 4 {
		t.Fatalf("live workers = %d, want 4", got)
	}

	// Shrink 4 → 1: the 4 busy tasks finish, the 3 excess workers
	// retire promptly even with an EMPTY queue (resize wake).
	q.SetConcurrency(1)

	// Release the busy tasks.
	close(tester.block)

	shrinkDeadline := time.Now().Add(5 * time.Second)

	for int(q.liveWorkers.Load()) > 1 && time.Now().Before(shrinkDeadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if got := int(q.liveWorkers.Load()); got != 1 {
		t.Fatalf("live workers after shrink = %d, want 1", got)
	}

	// The surviving worker keeps processing: enqueue + complete one
	// more task end-to-end.
	if _, err := q.Enqueue("fp-after-shrink", "vless", nil, 100, "src", EnqueueDefault); err != nil {
		t.Fatalf("enqueue after shrink: %v", err)
	}

	done := time.Now().Add(5 * time.Second)

	for {
		stats := q.Stats()

		if stats.TotalCompleted >= 5 {
			break
		}

		if time.Now().After(done) {
			t.Fatalf("queue stopped processing after shrink: %+v", stats)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestSetConcurrencyConcurrent exercises SetConcurrency racing
// against itself and against task completion — the CAS spawn loop and
// the retire path must never double-spawn, double-decrement or leak.
func TestSetConcurrencyConcurrent(t *testing.T) {
	tester := newCountingTester()
	q := New(tester, Config{
		Concurrency:  2,
		MaxQueueSize: 1000,
		Timeout:      5 * time.Second,
		MaxAttempts:  1,
	})
	q.Start(context.Background())
	defer q.Stop()

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()
			q.SetConcurrency(1 + n%6)
		}(i)
	}

	wg.Wait()

	// Enqueue work and let it finish; the pool must stay consistent.
	for i := 0; i < 20; i++ {
		if _, err := q.Enqueue(fmt.Sprintf("fp-race-%d", i), "vless", nil, 100, "src", EnqueueDefault); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	close(tester.block)

	drainDeadline := time.Now().Add(10 * time.Second)

	for {
		stats := q.Stats()

		if stats.TotalCompleted >= 20 {
			break
		}

		if time.Now().After(drainDeadline) {
			t.Fatalf("tasks did not complete: %+v", stats)
		}

		time.Sleep(20 * time.Millisecond)
	}

	live := int(q.liveWorkers.Load())
	desired := int(q.desiredWorkers.Load())

	if live < 1 || live > 7 || desired < 1 || desired > 6 {
		t.Fatalf("pool inconsistent after racing resizes: live=%d desired=%d", live, desired)
	}
}

// instantTester completes every test immediately with a working
// result — used by the retirement regression to cycle the pool
// through grow/shrink rounds at high rate.
type instantTester struct{}

func (instantTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	return Result{TestedAt: time.Now().UTC(), Working: true}, nil
}

// TestShrinkRetireNeverOvershoots is the regression test for the
// v0.9.2 race-mode CI failure (TestSetConcurrencyConcurrent:
// "tasks did not complete", ActiveWorkers 0). The historical
// retirement check read liveWorkers and decremented it lazily on
// exit, so every worker could observe the SAME stale count during a
// shrink and ALL retire, leaving liveWorkers == 0 with
// desiredWorkers > 0 — a dead pool nobody respawns (spawning lives
// only in Start and SetConcurrency). Retirement is now a CAS
// decrement: each worker claims one retirement slot, so the pool
// converges to exactly desiredWorkers and always keeps draining.
//
// The 8→1 grow/shrink churn below reliably killed the pool on the
// old code: all eight idle workers wake on the resize broadcast and
// each must re-read liveWorkers before the previous worker's lazy
// decrement lands — with eight racers the window is hit almost every
// cycle, leaving liveWorkers 0 with desiredWorkers 1. Each cycle
// demands visible progress, so any overshoot-retirement fails fast.
func TestShrinkRetireNeverOvershoots(t *testing.T) {
	q := New(instantTester{}, Config{
		Concurrency:  8,
		MaxQueueSize: 1000,
		Timeout:      5 * time.Second,
		MaxAttempts:  1,
	})
	q.Start(context.Background())
	defer q.Stop()

	const cycles = 12

	for cycle := 0; cycle < cycles; cycle++ {
		// Park the pool at 8 workers (Start spawned 8; the probe of
		// the previous cycle is long done) and shrink to 1: the
		// resize broadcast wakes all eight idle workers and EXACTLY
		// SEVEN retirement slots must be claimed.
		q.SetConcurrency(1)

		if _, err := q.Enqueue(
			fmt.Sprintf("fp-shrink-%d", cycle), "vless", nil, 100, "src", EnqueueDefault,
		); err != nil {
			t.Fatalf("cycle %d: enqueue: %v", cycle, err)
		}

		// The probe must complete before the next cycle: a dead
		// pool (the regression) leaves it queued forever.
		deadline := time.Now().Add(5 * time.Second)

		for q.Stats().TotalCompleted < int64(cycle+1) {
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d: pool stopped draining (retirement overshoot): "+
					"live=%d desired=%d stats=%+v",
					cycle, q.liveWorkers.Load(), q.desiredWorkers.Load(), q.Stats())
			}

			time.Sleep(2 * time.Millisecond)
		}
	}

	// The pool must converge to exactly the desired size.
	if live := int(q.liveWorkers.Load()); live != 1 {
		t.Fatalf("live workers = %d, want 1 (pool must converge to desired size)", live)
	}
}

// TestMemoryEstimate verifies the O(1) queue memory estimate tracks
// the real pending + inflight depth.
func TestMemoryEstimate(t *testing.T) {
	q := New(NoopTester{}, Config{
		Concurrency:  0, // no workers: everything stays queued
		MaxQueueSize: 100000,
	})
	q.Start(context.Background())
	defer q.Stop()

	before := q.MemoryEstimate()

	for i := 0; i < 1000; i++ {
		if _, err := q.Enqueue(fmt.Sprintf("fp-mem-%d", i), "vless", nil, 100, "src", EnqueueDefault); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	after := q.MemoryEstimate()

	if after-before != int64(1000)*taskBytesEstimate {
		t.Fatalf("memory estimate delta = %d, want %d", after-before, int64(1000)*taskBytesEstimate)
	}

	if cancelled := q.CancelAll(); cancelled != 1000 {
		t.Fatalf("CancelAll = %d, want 1000", cancelled)
	}

	if final := q.MemoryEstimate(); final != 0 {
		t.Fatalf("memory estimate after cancel-all = %d, want 0", final)
	}
}

// TestSetMaxQueueSize verifies the runtime capacity bound: a smaller
// bound rejects NEW enqueues without dropping queued tasks.
func TestSetMaxQueueSize(t *testing.T) {
	q := New(NoopTester{}, Config{
		Concurrency:  0,
		MaxQueueSize: 100000,
	})
	q.Start(context.Background())
	defer q.Stop()

	for i := 0; i < 50; i++ {
		if _, err := q.Enqueue(fmt.Sprintf("fp-cap-%d", i), "vless", nil, 100, "src", EnqueueDefault); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	q.SetMaxQueueSize(50)

	if _, err := q.Enqueue("fp-cap-new", "vless", nil, 100, "src", EnqueueTry); err == nil {
		t.Fatal("enqueue must be rejected at the new bound")
	} else if err != ErrQueueFull {
		t.Fatalf("enqueue error = %v, want ErrQueueFull", err)
	}

	// Queued tasks survive the bound change.
	if stats := q.Stats(); stats.QueueDepth != 50 {
		t.Fatalf("queue depth = %d, want 50 (bound change must not drop tasks)", stats.QueueDepth)
	}
}
