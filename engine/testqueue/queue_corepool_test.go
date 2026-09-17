package testqueue

// v0.9.7 tests (§14/§23): the bounded CORE-PROBE pool. Thousands of
// queued configurations must never imply uncontrolled core-process
// creation; the hard cap holds under bulk load, cancellation leaves
// no parked workers or leaked slots, and the adaptive concurrency
// knobs can never raise the cap above MaxCoreProbeConcurrency.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// coreTrackingTester counts concurrent in-flight Test calls (each one
// models a temporary protocol-core process).
type coreTrackingTester struct {
	delay     time.Duration
	inFlight  atomic.Int64
	peak      atomic.Int64
	total     atomic.Int64
	blockChan chan struct{}
}

func (c *coreTrackingTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	current := c.inFlight.Add(1)

	for {
		peak := c.peak.Load()
		if current <= peak || c.peak.CompareAndSwap(peak, current) {
			break
		}
	}

	c.total.Add(1)

	defer c.inFlight.Add(-1)

	if c.blockChan != nil {
		select {
		case <-c.blockChan:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}

	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	return Result{TestedAt: time.Now().UTC(), Working: true}, nil
}

// TestCoreProbePoolBoundedUnderBulkLoad verifies the hard cap: with
// 8 workers and 200 queued tasks, no more than 2 concurrent core
// processes ever run (default cap).
func TestCoreProbePoolBoundedUnderBulkLoad(t *testing.T) {
	tester := &coreTrackingTester{delay: 15 * time.Millisecond}

	q := New(tester, Config{
		Concurrency:  8,
		MaxQueueSize: 1000,
		Timeout:      5 * time.Second,
		MaxAttempts:  1,
		// CoreProbeConcurrency unset → default 2.
	})
	q.Start(context.Background())
	defer q.Stop()

	for i := 0; i < 200; i++ {
		if _, err := q.Enqueue(fmt.Sprintf("fp-bulk-%d", i), "vless", nil, 100, "src", EnqueueDefault); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := q.Drain(drainCtx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	stats := q.Stats()

	if got := tester.peak.Load(); got > 2 {
		t.Fatalf("peak concurrent core processes = %d, want ≤ 2", got)
	}

	if stats.TotalCompleted != 200 {
		t.Fatalf("completed = %d, want 200", stats.TotalCompleted)
	}

	if stats.CoreProbeConcurrency != DefaultCoreProbeConcurrency {
		t.Fatalf("reported cap = %d, want %d", stats.CoreProbeConcurrency, DefaultCoreProbeConcurrency)
	}

	if stats.ActiveCores != 0 {
		t.Fatalf("ActiveCores = %d after drain, want 0", stats.ActiveCores)
	}
}

// TestCoreProbeCapIsAbsoluteCeiling verifies no configuration can
// raise the core-probe cap above MaxCoreProbeConcurrency (4) — memory
// availability must not automatically create more core processes.
func TestCoreProbeCapIsAbsoluteCeiling(t *testing.T) {
	q := New(NoopTester{}, Config{
		Concurrency:          16,
		CoreProbeConcurrency: 64, // absurd value must clamp to the ceiling
	})

	if q.config.CoreProbeConcurrency != MaxCoreProbeConcurrency {
		t.Fatalf("cap = %d, want clamped to %d", q.config.CoreProbeConcurrency, MaxCoreProbeConcurrency)
	}
}

// TestCoreProbeCapNeverRaisesWithWorkers verifies growing the worker
// pool leaves the core-probe cap untouched (workers park instead).
func TestCoreProbeCapNeverRaisesWithWorkers(t *testing.T) {
	tester := &coreTrackingTester{delay: 20 * time.Millisecond}

	q := New(tester, Config{
		Concurrency:  2,
		MaxQueueSize: 1000,
		Timeout:      5 * time.Second,
		MaxAttempts:  1,
	})
	q.Start(context.Background())
	defer q.Stop()

	// Simulate adaptive growth far beyond the core cap.
	q.SetConcurrency(16)

	if got := q.Stats().CoreProbeConcurrency; got != DefaultCoreProbeConcurrency {
		t.Fatalf("cap after SetConcurrency = %d, want %d", got, DefaultCoreProbeConcurrency)
	}
}

// TestCoreProbeCancellationLeavesNoLeakedSlots verifies cancelling a
// bulk run while tasks are parked on the core pool unwinds cleanly:
// all tasks reach a terminal state, no slot stays held.
func TestCoreProbeCancellationLeavesNoLeakedSlots(t *testing.T) {
	tester := &coreTrackingTester{delay: 50 * time.Millisecond}

	q := New(tester, Config{
		Concurrency:  8,
		MaxQueueSize: 1000,
		Timeout:      5 * time.Second,
		MaxAttempts:  1,
	})
	q.Start(context.Background())
	defer q.Stop()

	for i := 0; i < 100; i++ {
		if _, err := q.Enqueue(fmt.Sprintf("fp-cancel-%d", i), "vless", nil, 100, "src-bulk", EnqueueDefault); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// Let a few start, then cancel the whole run mid-flight.
	time.Sleep(120 * time.Millisecond)

	// A handful may already have finished before the cancel landed.
	if got := q.CancelBySource("src-bulk"); got == 0 {
		t.Fatal("cancelled = 0, want the still-queued/in-flight tasks")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := q.Drain(drainCtx); err != nil {
		t.Fatalf("drain after cancel: %v", err)
	}

	// Cancelled in-flight tests unwind asynchronously (the tester
	// observes ctx cancellation, then releases its slot); wait a
	// bounded moment for the counters to settle.
	settleDeadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(settleDeadline) {
		if q.Stats().ActiveCores == 0 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	stats := q.Stats()

	if stats.ActiveCores != 0 {
		t.Fatalf("ActiveCores = %d after cancellation, want 0 (leaked slot)", stats.ActiveCores)
	}

	if got := tester.peak.Load(); got > 2 {
		t.Fatalf("peak concurrent cores = %d, want ≤ 2 even during cancellation", got)
	}
}

// TestCoreProbeSlotsReused verifies worker reuse: many tasks flow
// through the same bounded slot set (total far exceeds cap).
func TestCoreProbeSlotsReused(t *testing.T) {
	tester := &coreTrackingTester{delay: time.Millisecond}

	q := New(tester, Config{
		Concurrency:  2,
		MaxQueueSize: 1000,
		Timeout:      5 * time.Second,
		MaxAttempts:  1,
	})
	q.Start(context.Background())
	defer q.Stop()

	var wg sync.WaitGroup

	for i := 0; i < 300; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			_, _ = q.Enqueue(fmt.Sprintf("fp-reuse-%d", i), "vless", nil, 100, "src", EnqueueDefault)
		}(i)
	}

	wg.Wait()

	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := q.Drain(drainCtx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if got := tester.total.Load(); got != 300 {
		t.Fatalf("total tests = %d, want 300 (slots must be reusable)", got)
	}
}
