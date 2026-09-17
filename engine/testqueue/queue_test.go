package testqueue

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTester is a controllable Tester for unit tests.
type fakeTester struct {
	calls    atomic.Int64
	delay    time.Duration
	outcomes map[string]Result // fingerprint → outcome
	mu       sync.Mutex
	// onTest, if set, is called before each Test invocation.
	onTest func(fp string)
}

func (f *fakeTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	f.calls.Add(1)

	f.mu.Lock()
	if f.onTest != nil {
		f.onTest(fp)
	}
	outcomes := f.outcomes
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return Result{LastError: "cancelled"}, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if r, ok := outcomes[fp]; ok {
		return r, nil
	}
	return Result{Working: true, Latency: 50 * time.Millisecond, TestedAt: time.Now().UTC()}, nil
}

func (f *fakeTester) setOutcome(fp string, r Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outcomes == nil {
		f.outcomes = make(map[string]Result)
	}
	f.outcomes[fp] = r
}

// TestEnqueueDequeue verifies basic queue mechanics.
func TestEnqueueDequeue(t *testing.T) {
	q := New(&fakeTester{}, Config{Concurrency: 1, Timeout: 5 * time.Second, MaxAttempts: 1, MaxQueueSize: 100})
	q.Start(context.Background())
	defer q.Stop()

	id, err := q.Enqueue("fp-1", "vless", []string{"xray"}, 100, "src-1", EnqueueDefault)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == 0 {
		t.Fatalf("Enqueue returned id 0")
	}

	// Give the worker a moment to pick it up.
	time.Sleep(100 * time.Millisecond)

	stats := q.Stats()
	if stats.TotalEnqueued != 1 {
		t.Errorf("TotalEnqueued = %d, want 1", stats.TotalEnqueued)
	}
	if stats.TotalCompleted != 1 {
		t.Errorf("TotalCompleted = %d, want 1", stats.TotalCompleted)
	}
	if stats.TotalPassed != 1 {
		t.Errorf("TotalPassed = %d, want 1", stats.TotalPassed)
	}
}

// TestDuplicateSuppression verifies the queue refuses duplicates.
func TestDuplicateSuppression(t *testing.T) {
	q := New(&fakeTester{delay: 200 * time.Millisecond}, Config{Concurrency: 0, MaxQueueSize: 100, Timeout: 5 * time.Second, MaxAttempts: 1})
	// Don't start workers; the task stays queued.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)
	defer q.Stop()

	_, _ = q.Enqueue("fp-dup", "vless", []string{"xray"}, 100, "src", EnqueueDefault)
	_, err := q.Enqueue("fp-dup", "vless", []string{"xray"}, 100, "src", EnqueueDefault)
	if err != ErrDuplicate {
		t.Errorf("second Enqueue returned err = %v, want ErrDuplicate", err)
	}
}

// TestCancelBySource verifies source-scoped cancellation. With
// Concurrency=0 (no workers), tasks remain queued so CancelBySource
// finds them in pending.
func TestCancelBySource(t *testing.T) {
	q := New(&fakeTester{delay: 1 * time.Hour}, Config{Concurrency: 0, MaxQueueSize: 100, Timeout: 5 * time.Second, MaxAttempts: 1})
	// No workers started; tasks remain queued.
	q.Start(context.Background())
	defer q.Stop()

	q.Enqueue("fp-1", "vless", nil, 100, "src-A", EnqueueDefault)
	q.Enqueue("fp-2", "vmess", nil, 100, "src-A", EnqueueDefault)
	q.Enqueue("fp-3", "trojan", nil, 100, "src-B", EnqueueDefault)

	cancelled := q.CancelBySource("src-A")
	if cancelled != 2 {
		t.Errorf("cancelled = %d, want 2", cancelled)
	}

	stats := q.Stats()
	if stats.TotalCancelled != 2 {
		t.Errorf("TotalCancelled = %d, want 2", stats.TotalCancelled)
	}

	// src-B task should still be queued.
	if stats.QueueDepth != 1 {
		t.Errorf("QueueDepth = %d, want 1", stats.QueueDepth)
	}
}

// TestModeConfig verifies every mode presets a sane configuration.
func TestModeConfig(t *testing.T) {
	for _, mode := range []Mode{ModeQuick, ModeBalanced, ModeDeep, ModeRetestFailed, ModeTestAll, ModeTestSelected, ModeContinuous} {
		cfg := ModeConfig(mode)
		if cfg.Concurrency <= 0 {
			t.Errorf("mode %s: Concurrency = 0", mode)
		}
		if cfg.Timeout <= 0 {
			t.Errorf("mode %s: Timeout = 0", mode)
		}
		if cfg.MaxAttempts <= 0 {
			t.Errorf("mode %s: MaxAttempts = 0", mode)
		}
	}
}

// TestFailureClassification verifies the classifier maps common
// errors to expected categories.
func TestFailureClassification(t *testing.T) {
	// Set up contexts that already have the corresponding error
	// stored, so classifyFailure sees ctx.Err() != nil.
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	time.Sleep(2 * time.Millisecond) // ensure the deadline has elapsed
	defer deadlineCancel()

	cancelledCtx, cancelledCancel := context.WithCancel(context.Background())
	cancelledCancel()

	cases := []struct {
		name string
		err  error
		ctx  context.Context
		want FailureCategory
	}{
		{"nil-error", nil, context.Background(), FailureNone},
		{"deadline-exceeded", context.DeadlineExceeded, deadlineCtx, FailureTimeout},
		{"cancelled", context.Canceled, cancelledCtx, FailureCancelled},
	}
	for _, c := range cases {
		got := classifyFailure(c.err, c.ctx)
		if got != c.want {
			t.Errorf("classifyFailure(%s) = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestStateIsTerminal verifies the terminal-state predicate.
func TestStateIsTerminal(t *testing.T) {
	terminal := []TaskState{StatePassed, StateFailed, StateTimedOut, StateCancelled}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("state %s should be terminal", s)
		}
	}
	transient := []TaskState{StateQueued, StatePreparing, StateTesting, StateMeasuring}
	for _, s := range transient {
		if s.IsTerminal() {
			t.Errorf("state %s should NOT be terminal", s)
		}
	}
}

// --- Stress / regression tests for the cancellation path ---
//
// These tests exercise the cancellation state machine under
// concurrency and timing variability. They must pass reliably under
// `go test -race -count=N` for any reasonable N.

// TestCancelBySourceQueuedOnly: CancelBySource with only queued tasks
// (no workers). Every matching task must be cancelled exactly once.
func TestCancelBySourceQueuedOnly(t *testing.T) {
	q := New(&fakeTester{delay: 1 * time.Hour}, Config{Concurrency: 0, MaxQueueSize: 1000, Timeout: 5 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	for i := 0; i < 50; i++ {
		q.Enqueue(fmt.Sprintf("fp-A-%d", i), "vless", nil, 100, "src-A", EnqueueDefault)
	}
	for i := 0; i < 30; i++ {
		q.Enqueue(fmt.Sprintf("fp-B-%d", i), "vmess", nil, 100, "src-B", EnqueueDefault)
	}

	cancelled := q.CancelBySource("src-A")
	if cancelled != 50 {
		t.Errorf("cancelled = %d, want 50", cancelled)
	}
	stats := q.Stats()
	if stats.TotalCancelled != 50 {
		t.Errorf("TotalCancelled = %d, want 50", stats.TotalCancelled)
	}
	if stats.QueueDepth != 30 {
		t.Errorf("QueueDepth = %d, want 30", stats.QueueDepth)
	}

	// src-B tasks can still be cancelled.
	cancelled = q.CancelBySource("src-B")
	if cancelled != 30 {
		t.Errorf("second CancelBySource cancelled = %d, want 30", cancelled)
	}
}

// TestCancelBySourceWhileWorkersDequeue: workers are actively
// dequeuing. CancelBySource must cancel exactly the matching tasks
// that haven't already completed, and TotalCancelled must be
// consistent with the terminal-state count.
func TestCancelBySourceWhileWorkersDequeue(t *testing.T) {
	tester := &fakeTester{delay: 50 * time.Millisecond}
	q := New(tester, Config{Concurrency: 4, MaxQueueSize: 1000, Timeout: 5 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	// Enqueue 200 tasks from src-A and 200 from src-B.
	for i := 0; i < 200; i++ {
		q.Enqueue(fmt.Sprintf("fp-A-%d", i), "vless", nil, 100, "src-A", EnqueueDefault)
		q.Enqueue(fmt.Sprintf("fp-B-%d", i), "vmess", nil, 100, "src-B", EnqueueDefault)
	}

	// Let workers start processing.
	time.Sleep(30 * time.Millisecond)

	cancelled := q.CancelBySource("src-A")
	stats := q.Stats()

	// Invariants: TotalCompleted == TotalPassed + TotalFailed + TotalTimedOut + TotalCancelled.
	total := stats.TotalPassed + stats.TotalFailed + stats.TotalTimedOut + stats.TotalCancelled
	if total != stats.TotalCompleted {
		t.Errorf("terminal sum %d != TotalCompleted %d", total, stats.TotalCompleted)
	}

	// cancelled is at most 200 (some may have already completed before we cancelled).
	if cancelled > 200 {
		t.Errorf("cancelled = %d, should be <= 200", cancelled)
	}

	// Wait for the queue to drain.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	q.Drain(drainCtx)

	stats = q.Stats()
	total = stats.TotalPassed + stats.TotalFailed + stats.TotalTimedOut + stats.TotalCancelled
	if total != stats.TotalCompleted {
		t.Errorf("post-drain terminal sum %d != TotalCompleted %d", total, stats.TotalCompleted)
	}
	if stats.TotalCompleted != 400 {
		t.Errorf("TotalCompleted = %d, want 400", stats.TotalCompleted)
	}
	// All src-A tasks either completed (before cancel) or were cancelled.
	// All src-B tasks completed.
	if int(stats.TotalCancelled) > 200 {
		t.Errorf("TotalCancelled = %d, should be <= 200", stats.TotalCancelled)
	}
}

// TestCancelBySourceWhileTasksRunning: in-flight tests are cancelled
// via per-task context. The tester must observe ctx.Done().
//
// v0.9.7: with the bounded core-probe pool (default cap 2), exactly
// the tasks HOLDING a core slot run the tester and observe per-task
// context cancellation; tasks parked on the slot semaphore detect the
// cancelled state at their next poll and return WITHOUT spawning a
// core — the queue cancels everything while only 2 temporary core
// processes ever exist.
func TestCancelBySourceWhileTasksRunning(t *testing.T) {
	var observedCancellation atomic.Int64
	tester := &fakeTester{delay: 5 * time.Second}
	tester.mu.Lock()
	tester.onTest = func(fp string) {
		// nothing here; the delay itself blocks until ctx is cancelled
	}
	tester.mu.Unlock()

	// We need to detect that the tester observed a cancellation. Use a
	// custom tester via a wrapper.
	wrapped := &cancellationObservingTester{
		inner:    tester,
		observed: &observedCancellation,
	}

	q := New(wrapped, Config{Concurrency: 4, MaxQueueSize: 100, Timeout: 30 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	for i := 0; i < 4; i++ {
		q.Enqueue(fmt.Sprintf("fp-run-%d", i), "vless", nil, 100, "src-run", EnqueueDefault)
	}

	// Wait for the slot-holding tasks to enter the test.
	time.Sleep(100 * time.Millisecond)

	cancelled := q.CancelBySource("src-run")
	if cancelled != 4 {
		t.Errorf("cancelled = %d, want 4", cancelled)
	}

	// The tester's 5s delay should be interrupted by the per-task
	// context cancellation for the in-flight (slot-holding) tasks.
	// Parked tasks are cancelled without ever reaching the tester.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if observedCancellation.Load() == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := observedCancellation.Load(); got != 2 {
		t.Errorf("observedCancellation = %d, want 2 (in-flight ctx cancelled; parked tasks must not spawn)", got)
	}

	stats := q.Stats()
	if stats.TotalCancelled != 4 {
		t.Errorf("TotalCancelled = %d, want 4", stats.TotalCancelled)
	}
	if stats.ActiveCores != 0 {
		t.Errorf("ActiveCores = %d, want 0 (no leaked core slots)", stats.ActiveCores)
	}
}

// cancellationObservingTester wraps a Tester and counts how many
// times the context was cancelled during Test.
type cancellationObservingTester struct {
	inner    Tester
	observed *atomic.Int64
}

func (c *cancellationObservingTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	r, err := c.inner.Test(ctx, fp, backends)
	if ctx.Err() != nil {
		c.observed.Add(1)
	}
	return r, err
}

// perFingerprintTester returns a different result/delay per fingerprint.
type perFingerprintTester struct {
	specs map[string]struct {
		delay  time.Duration
		result Result
		err    error
	}
}

func (p *perFingerprintTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	spec, ok := p.specs[fp]
	if !ok {
		return Result{Working: true, Latency: 10 * time.Millisecond, TestedAt: time.Now().UTC()}, nil
	}
	if spec.delay > 0 {
		select {
		case <-ctx.Done():
			return Result{LastError: "cancelled"}, ctx.Err()
		case <-time.After(spec.delay):
		}
	}
	return spec.result, spec.err
}

// TestCancelByFingerprintDuringExecution: cancel a specific task by
// fingerprint while it is running. The cancelled task's test context
// is cancelled so it returns promptly; a queued sibling completes
// normally.
func TestCancelByFingerprintDuringExecution(t *testing.T) {
	tester := &perFingerprintTester{specs: map[string]struct {
		delay  time.Duration
		result Result
		err    error
	}{
		"fp-target": {delay: 30 * time.Second, result: Result{Working: true, Latency: 30 * time.Second}},
		"fp-other":  {delay: 10 * time.Millisecond, result: Result{Working: true, Latency: 10 * time.Millisecond}},
	}}
	q := New(tester, Config{Concurrency: 1, MaxQueueSize: 100, Timeout: 60 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	q.Enqueue("fp-target", "vless", nil, 100, "src", EnqueueDefault)
	q.Enqueue("fp-other", "vmess", nil, 100, "src", EnqueueDefault)

	// Let the worker pick up fp-target (the 30s test starts).
	time.Sleep(100 * time.Millisecond)

	got := q.CancelByFingerprint("fp-target")
	if got != 1 {
		t.Errorf("CancelByFingerprint = %d, want 1", got)
	}

	// fp-target's 30s test should be interrupted; fp-other runs fast.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer drainCancel()
	if err := q.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	stats := q.Stats()
	// fp-target cancelled, fp-other passed.
	if stats.TotalCancelled != 1 {
		t.Errorf("TotalCancelled = %d, want 1", stats.TotalCancelled)
	}
	if stats.TotalPassed != 1 {
		t.Errorf("TotalPassed = %d, want 1", stats.TotalPassed)
	}
}

// TestGlobalCancellation: CancelAll cancels every pending + in-flight task.
func TestGlobalCancellation(t *testing.T) {
	tester := &fakeTester{delay: 5 * time.Second}
	q := New(tester, Config{Concurrency: 4, MaxQueueSize: 1000, Timeout: 30 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	for i := 0; i < 100; i++ {
		q.Enqueue(fmt.Sprintf("fp-g-%d", i), "vless", nil, 100, "src", EnqueueDefault)
	}

	time.Sleep(100 * time.Millisecond) // let some workers start

	cancelled := q.CancelAll()
	stats := q.Stats()

	if int64(cancelled) != int64(stats.TotalCancelled) {
		t.Errorf("CancelAll returned %d but TotalCancelled = %d", cancelled, stats.TotalCancelled)
	}

	// After CancelAll, the queue should be empty (pending=0). Inflight
	// tasks are cancelled but may take a moment to actually exit.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer drainCancel()
	q.Drain(drainCtx)

	stats = q.Stats()
	total := stats.TotalPassed + stats.TotalFailed + stats.TotalTimedOut + stats.TotalCancelled
	if total != stats.TotalCompleted {
		t.Errorf("terminal sum %d != TotalCompleted %d", total, stats.TotalCompleted)
	}
	if stats.TotalCompleted != 100 {
		t.Errorf("TotalCompleted = %d, want 100", stats.TotalCompleted)
	}
}

// TestCancellationDuringRetryBackoff: a task that fails and enters
// backoff must be cancellable. The cancel transitions it to
// StateCancelled even while the worker is sleeping in backoff.
func TestCancellationDuringRetryBackoff(t *testing.T) {
	// Tester that fails fast with a network error so the task enters
	// retry backoff (1s for the first backoff).
	tester := &perFingerprintTester{specs: map[string]struct {
		delay  time.Duration
		result Result
		err    error
	}{
		"fp-retry": {delay: 5 * time.Millisecond, result: Result{LastError: "connection refused"}, err: fmt.Errorf("connection refused")},
	}}
	q := New(tester, Config{Concurrency: 1, MaxQueueSize: 100, Timeout: 1 * time.Second, MaxAttempts: 5})
	q.Start(context.Background())
	defer q.Stop()

	q.Enqueue("fp-retry", "vless", nil, 100, "src", EnqueueDefault)

	// Wait for the first attempt to fail and backoff (1s) to start.
	time.Sleep(100 * time.Millisecond)

	// Cancel by fingerprint — the task is in backoff (not in tester.Test).
	got := q.CancelByFingerprint("fp-retry")
	if got != 1 {
		t.Errorf("CancelByFingerprint during backoff = %d, want 1", got)
	}

	// The worker is sleeping in the backoff select. It will only exit
	// when q.ctx is cancelled (Stop) or the backoff timer fires. Since
	// we don't want to wait 1s, we Stop the queue (which cancels q.ctx
	// and the backoff select returns via ctx.Done → finishTask
	// returns false because already cancelled). The worker exits.
	stopDone := make(chan struct{})
	go func() {
		q.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop took too long")
	}

	stats := q.Stats()
	if stats.TotalCancelled != 1 {
		t.Errorf("TotalCancelled = %d, want 1", stats.TotalCancelled)
	}
}

// TestCancellationDuringTimeout: a task whose test times out must
// reach StateTimedOut, and a concurrent cancel must not double-count.
func TestCancellationDuringTimeout(t *testing.T) {
	tester := &fakeTester{delay: 2 * time.Second}
	q := New(tester, Config{Concurrency: 1, MaxQueueSize: 100, Timeout: 200 * time.Millisecond, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	q.Enqueue("fp-to", "vless", nil, 100, "src", EnqueueDefault)

	// Wait for the timeout to fire.
	time.Sleep(400 * time.Millisecond)

	// Cancel after the timeout — should be a no-op (already terminal).
	got := q.CancelByFingerprint("fp-to")
	if got != 0 {
		t.Errorf("Cancel after terminal = %d, want 0", got)
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer drainCancel()
	q.Drain(drainCtx)

	stats := q.Stats()
	if stats.TotalTimedOut != 1 {
		t.Errorf("TotalTimedOut = %d, want 1", stats.TotalTimedOut)
	}
	if stats.TotalCancelled != 0 {
		t.Errorf("TotalCancelled = %d, want 0", stats.TotalCancelled)
	}
}

// TestCancellationDuringShutdown: Stop cancels in-flight tests via
// the queue context. Workers exit promptly.
func TestCancellationDuringShutdown(t *testing.T) {
	tester := &fakeTester{delay: 30 * time.Second}
	q := New(tester, Config{Concurrency: 4, MaxQueueSize: 100, Timeout: 60 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())

	for i := 0; i < 8; i++ {
		q.Enqueue(fmt.Sprintf("fp-sd-%d", i), "vless", nil, 100, "src", EnqueueDefault)
	}

	// Let workers pick up tasks.
	time.Sleep(100 * time.Millisecond)

	// Stop must return promptly (within a few seconds), not wait 30s.
	done := make(chan struct{})
	go func() {
		q.Stop()
		close(done)
	}()
	select {
	case <-done:
		// good
	case <-time.After(5 * time.Second):
		t.Fatal("Stop took >5s; in-flight tests were not cancelled by shutdown")
	}
}

// TestConcurrentEnqueueCancel: many goroutines enqueue and cancel
// concurrently. Final counters must be consistent.
//
// v0.9.7: CoreProbeConcurrency = 4 keeps the pre-0.9.7 throughput for
// this consistency test (the bounded-pool behavior is covered by the
// dedicated core-probe tests).
func TestConcurrentEnqueueCancel(t *testing.T) {
	tester := &fakeTester{delay: 10 * time.Millisecond}
	q := New(tester, Config{Concurrency: 4, MaxQueueSize: 10000, Timeout: 1 * time.Second, MaxAttempts: 1, CoreProbeConcurrency: 4})
	q.Start(context.Background())
	defer q.Stop()

	var wg sync.WaitGroup
	var enqueued atomic.Int64
	var cancelled atomic.Int64

	// 8 enqueuers.
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				fp := fmt.Sprintf("fp-%d-%d", workerID, i)
				_, err := q.Enqueue(fp, "vless", nil, 100, "src", EnqueueDefault)
				if err == nil {
					enqueued.Add(1)
				}
			}
		}(w)
	}

	// 4 cancellers.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				fp := fmt.Sprintf("fp-%d-%d", workerID, i)
				if q.CancelByFingerprint(fp) == 1 {
					cancelled.Add(1)
				}
			}
		}(w)
	}

	wg.Wait()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer drainCancel()
	q.Drain(drainCtx)

	stats := q.Stats()
	total := stats.TotalPassed + stats.TotalFailed + stats.TotalTimedOut + stats.TotalCancelled
	if total != stats.TotalCompleted {
		t.Errorf("terminal sum %d != TotalCompleted %d", total, stats.TotalCompleted)
	}
	if stats.TotalEnqueued != enqueued.Load() {
		t.Errorf("TotalEnqueued = %d, enqueued counter = %d", stats.TotalEnqueued, enqueued.Load())
	}
	// Every task is either completed (passed/failed/timedout) or cancelled.
	if stats.TotalCompleted != stats.TotalEnqueued {
		t.Errorf("TotalCompleted %d != TotalEnqueued %d (lost tasks)", stats.TotalCompleted, stats.TotalEnqueued)
	}
}

// TestDuplicateSuppressionAndCancel: a duplicate Enqueue is rejected,
// and the original task can still be cancelled.
func TestDuplicateSuppressionAndCancel(t *testing.T) {
	q := New(&fakeTester{delay: 1 * time.Hour}, Config{Concurrency: 0, MaxQueueSize: 100, Timeout: 5 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	id1, err := q.Enqueue("fp-dup", "vless", nil, 100, "src", EnqueueDefault)
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	_, err = q.Enqueue("fp-dup", "vless", nil, 100, "src", EnqueueDefault)
	if err != ErrDuplicate {
		t.Errorf("second Enqueue err = %v, want ErrDuplicate", err)
	}

	got := q.CancelByFingerprint("fp-dup")
	if got != 1 {
		t.Errorf("Cancel = %d, want 1", got)
	}
	stats := q.Stats()
	if stats.TotalCancelled != 1 {
		t.Errorf("TotalCancelled = %d, want 1", stats.TotalCancelled)
	}
	// After cancel, the fingerprint is free again.
	id2, err := q.Enqueue("fp-dup", "vless", nil, 100, "src", EnqueueDefault)
	if err != nil {
		t.Errorf("re-Enqueue after cancel: %v", err)
	}
	if id2 == id1 {
		t.Errorf("re-Enqueue returned same ID %d", id2)
	}
}

// TestRepeatedCancellation: cancelling the same task multiple times
// only increments TotalCancelled once.
func TestRepeatedCancellation(t *testing.T) {
	q := New(&fakeTester{delay: 1 * time.Hour}, Config{Concurrency: 0, MaxQueueSize: 100, Timeout: 5 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	q.Enqueue("fp-rep", "vless", nil, 100, "src", EnqueueDefault)

	for i := 0; i < 10; i++ {
		q.CancelByFingerprint("fp-rep")
	}
	stats := q.Stats()
	if stats.TotalCancelled != 1 {
		t.Errorf("TotalCancelled = %d, want 1 (idempotent)", stats.TotalCancelled)
	}
}

// TestReplaceIfHigher: EnqueueReplaceIfHigher updates priority on an
// existing queued task and the heap maintains order.
func TestReplaceIfHigher(t *testing.T) {
	q := New(&fakeTester{delay: 1 * time.Hour}, Config{Concurrency: 0, MaxQueueSize: 100, Timeout: 5 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	q.Enqueue("fp-low", "vless", nil, 100, "src", EnqueueDefault)
	q.Enqueue("fp-high", "vless", nil, 100, "src", EnqueueDefault)

	// Bump fp-low's priority above fp-high.
	_, err := q.Enqueue("fp-low", "vless", nil, 500, "src", EnqueueReplaceIfHigher)
	if err != ErrDuplicate {
		t.Errorf("ReplaceIfHigher err = %v, want ErrDuplicate", err)
	}

	snap := q.Snapshot(10)
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Fingerprint != "fp-low" || snap[0].Priority != 500 {
		t.Errorf("top task = %s/%d, want fp-low/500", snap[0].Fingerprint, snap[0].Priority)
	}

	// Lower-priority Enqueue should NOT replace.
	_, err = q.Enqueue("fp-low", "vless", nil, 50, "src", EnqueueReplaceIfHigher)
	if err != ErrDuplicate {
		t.Errorf("lower-priority ReplaceIfHigher err = %v, want ErrDuplicate", err)
	}
	snap = q.Snapshot(10)
	if snap[0].Priority != 500 {
		t.Errorf("priority changed to %d after lower-priority enqueue", snap[0].Priority)
	}
}

// TestStatsActiveWorkers: ActiveWorkers reflects inflight count.
func TestStatsActiveWorkers(t *testing.T) {
	tester := &fakeTester{delay: 500 * time.Millisecond}
	q := New(tester, Config{Concurrency: 4, MaxQueueSize: 100, Timeout: 5 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	for i := 0; i < 4; i++ {
		q.Enqueue(fmt.Sprintf("fp-aw-%d", i), "vless", nil, 100, "src", EnqueueDefault)
	}
	time.Sleep(100 * time.Millisecond) // let workers pick up

	stats := q.Stats()
	if stats.ActiveWorkers != 4 {
		t.Errorf("ActiveWorkers = %d, want 4", stats.ActiveWorkers)
	}
}

// TestStopWithoutStart: Stop on a never-started queue is a safe no-op.
func TestStopWithoutStart(t *testing.T) {
	q := New(NoopTester{}, DefaultConfig())
	q.Stop() // must not panic
}

// TestEnqueueAfterStop: Enqueue after Stop returns ErrQueueStopped.
func TestEnqueueAfterStop(t *testing.T) {
	q := New(NoopTester{}, Config{Concurrency: 1, Timeout: 1 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	q.Stop()

	_, err := q.Enqueue("fp-x", "vless", nil, 100, "src", EnqueueDefault)
	if err != ErrQueueStopped {
		t.Errorf("err = %v, want ErrQueueStopped", err)
	}
}

// TestQueueFull: EnqueueTry returns ErrQueueFull at capacity.
func TestQueueFull(t *testing.T) {
	q := New(&fakeTester{delay: 1 * time.Hour}, Config{Concurrency: 0, MaxQueueSize: 5, Timeout: 5 * time.Second, MaxAttempts: 1})
	q.Start(context.Background())
	defer q.Stop()

	for i := 0; i < 5; i++ {
		if _, err := q.Enqueue(fmt.Sprintf("fp-f-%d", i), "vless", nil, 100, "src", EnqueueTry); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	_, err := q.Enqueue("fp-overflow", "vless", nil, 100, "src", EnqueueTry)
	if err != ErrQueueFull {
		t.Errorf("Enqueue at capacity err = %v, want ErrQueueFull", err)
	}
}
