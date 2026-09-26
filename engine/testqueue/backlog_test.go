package testqueue

// v0.11.0 regression tests: bounded batch admission (the 10,000-task
// burst is gone), memory-aware admission backpressure, cancellation
// semantics over the backlog, and the bounded bulk-test progress
// aggregation contract.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// stubTester completes every test instantly with a fixed outcome.
type stubTester struct {
	working bool
	calls   atomic.Int64
}

func (s *stubTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	s.calls.Add(1)

	return Result{Working: s.working, TestedAt: time.Now().UTC()}, nil
}

// blockingTester parks every test until the test releases the gate —
// used to hold tasks in-flight deterministically.
type blockingTester struct {
	release chan struct{}
	started chan struct{}
	calls   atomic.Int64
}

func (b *blockingTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	b.calls.Add(1)

	select {
	case b.started <- struct{}{}:
	default:
	}

	select {
	case <-b.release:
		return Result{Working: true, TestedAt: time.Now().UTC()}, nil
	case <-ctx.Done():
		return Result{TestedAt: time.Now().UTC(), FailureCategory: FailureCancelled}, ctx.Err()
	}
}

func candidates(n int) []BacklogCandidate {
	out := make([]BacklogCandidate, 0, n)

	for i := 0; i < n; i++ {
		out = append(out, BacklogCandidate{Fingerprint: "fp-" + string(rune('a'+i%26)) + time.Now().Add(time.Duration(i)).Format("150405.000000000")})
	}

	return out
}

func fpList(n int) []BacklogCandidate {
	out := make([]BacklogCandidate, 0, n)

	for i := 0; i < n; i++ {
		out = append(out, BacklogCandidate{Fingerprint: fpn(i)})
	}

	return out
}

func fpn(i int) string {
	const digits = "0123456789abcdef"

	return "fingerprint-" + string(digits[(i>>8)&0xf]) + string(digits[(i>>4)&0xf]) + string(digits[i&0xf]) + "-0000"
}

// TestBacklogBoundedAdmission pins the v0.11.0 acceptance condition:
// a huge plan must NOT materialize as a huge task burst. With no
// workers, only the first admission batch (plus what the floor allows)
// becomes queued tasks; the rest stays deferred.
func TestBacklogBoundedAdmission(t *testing.T) {
	cfg := Config{
		Concurrency:    0, // no workers: nothing drains, admission is fully observable
		Timeout:        time.Second,
		MaxAttempts:    1,
		AdmissionBatch: 100,
		AdmissionFloor: 300,
	}

	q := New(&stubTester{}, cfg)

	accepted, materialized := q.EnqueueBacklog("batch-1", fpList(1000), 400)

	if accepted != 1000 {
		t.Fatalf("accepted = %d, want 1000", accepted)
	}

	if materialized != 100 {
		t.Fatalf("materialized = %d, want exactly the 100-task first batch", materialized)
	}

	if remaining := q.BacklogRemaining(); remaining != 900 {
		t.Fatalf("backlog remaining = %d, want 900", remaining)
	}

	if pending := q.Stats().QueueDepth; pending != 100 {
		t.Fatalf("pending = %d, want the bounded 100-task batch (no 1000-task burst)", pending)
	}
}

// TestBacklogDrainsByAdmittingNextBatches proves the pipeline:
// batch → execute → outcomes → next batch only when capacity permits.
func TestBacklogDrainsByAdmittingNextBatches(t *testing.T) {
	tester := &stubTester{working: true}

	cfg := Config{
		Concurrency:    4,
		Timeout:        time.Second,
		MaxAttempts:    1,
		AdmissionBatch: 50,
		AdmissionFloor: 120,
	}

	q := New(tester, cfg)
	q.Start(context.Background())
	defer q.Stop()

	q.OpenBatch("batch-drain")
	accepted, _ := q.EnqueueBacklog("batch-drain", fpList(500), 400)

	if accepted != 500 {
		t.Fatalf("accepted = %d, want 500", accepted)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := q.Drain(drainCtx); err != nil {
		t.Fatalf("queue did not drain the full plan: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for q.BacklogRemaining() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if remaining := q.BacklogRemaining(); remaining != 0 {
		t.Fatalf("backlog remaining = %d, want 0 (full plan must execute)", remaining)
	}

	if calls := tester.calls.Load(); calls < 500 {
		t.Fatalf("tester ran %d times, want >= 500 (every planned candidate executes)", calls)
	}
}

// TestBacklogAdmissionHeldUnderPressure pins the memory-aware gate:
// PauseAdmission stops admitting; ResumeAdmission continues. Critical
// memory must stop the queue from refilling while it drains.
func TestBacklogAdmissionHeldUnderPressure(t *testing.T) {
	cfg := Config{
		Concurrency:    0,
		Timeout:        time.Second,
		MaxAttempts:    1,
		AdmissionBatch: 100,
		AdmissionFloor: 300,
	}

	q := New(&stubTester{}, cfg)

	q.EnqueueBacklog("batch-1", fpList(600), 400)

	q.PauseAdmission()

	// Force another admission step attempt: pause must hold.
	if n := q.admit(); n != 0 {
		t.Fatalf("admitted %d tasks while admission was held, want 0", n)
	}

	if !q.AdmissionHeld() {
		t.Fatal("AdmissionHeld = false, want true after PauseAdmission")
	}

	q.ResumeAdmission()

	if q.AdmissionHeld() {
		t.Fatal("AdmissionHeld = true, want false after ResumeAdmission")
	}

	if pending := q.Stats().QueueDepth; pending <= 100 {
		t.Fatalf("pending = %d, want > 100 after resume admitted more batches", pending)
	}
}

// TestBacklogCancelAllDropsPlan pins cancellation: CancelAll cancels
// live tasks AND drops every deferred candidate; the batch completion
// event reports the drop honestly.
func TestBacklogCancelAllDropsPlan(t *testing.T) {
	events := make(chan ProgressEvent, 8)

	cfg := Config{
		Concurrency:    0,
		Timeout:        time.Second,
		MaxAttempts:    1,
		AdmissionBatch: 100,
		AdmissionFloor: 300,
	}

	q := New(&stubTester{}, cfg)
	q.SetReporter(func(ev ProgressEvent) { events <- ev })

	q.OpenBatch("batch-cancel")
	q.EnqueueBacklog("batch-cancel", fpList(500), 400)

	if cancelled := q.CancelAll(); cancelled != 100 {
		t.Fatalf("cancelled = %d live tasks, want 100", cancelled)
	}

	if remaining := q.BacklogRemaining(); remaining != 0 {
		t.Fatalf("backlog remaining after CancelAll = %d, want 0", remaining)
	}

	// A cancellation emits one throttled progress record before the
	// completion record — read until the completion arrives.
	deadline := time.After(2 * time.Second)

	for {
		select {
		case ev := <-events:
			if ev.Phase != "complete" {
				continue
			}

			if ev.Dropped != 400 {
				t.Fatalf("dropped = %d, want the 400 never-materialized candidates", ev.Dropped)
			}

			if ev.Cancelled != 100 {
				t.Fatalf("cancelled = %d, want 100", ev.Cancelled)
			}

			return

		case <-deadline:
			t.Fatal("no batch completion event after CancelAll")
		}
	}
}

// TestBacklogDuplicateSuppression pins one-test-per-fingerprint across
// the live queue AND the deferred backlog.
func TestBacklogDuplicateSuppression(t *testing.T) {
	cfg := Config{
		Concurrency:    0,
		Timeout:        time.Second,
		MaxAttempts:    1,
		AdmissionBatch: 200,
		AdmissionFloor: 400,
	}

	q := New(&stubTester{}, cfg)

	cands := fpList(50)
	accepted1, _ := q.EnqueueBacklog("b1", cands, 400)
	accepted2, _ := q.EnqueueBacklog("b2", cands, 400) // same fingerprints

	if accepted1 != 50 {
		t.Fatalf("first accept = %d, want 50", accepted1)
	}

	if accepted2 != 0 {
		t.Fatalf("duplicate accept = %d, want 0 (one test per fingerprint)", accepted2)
	}
}

// TestBacklogProgressAggregationIsBounded pins the bounded-emission
// contract: repetitive bulk activity produces a bounded number of
// progress records, never one per task.
func TestBacklogProgressAggregationIsBounded(t *testing.T) {
	events := make(chan ProgressEvent, 64)

	tester := &stubTester{working: true}

	cfg := Config{
		Concurrency:    3,
		Timeout:        time.Second,
		MaxAttempts:    1,
		AdmissionBatch: 100,
		AdmissionFloor: 200,
	}

	q := New(tester, cfg)
	q.SetReporter(func(ev ProgressEvent) { events <- ev })

	q.Start(context.Background())
	defer q.Stop()

	q.OpenBatch("batch-aggregate")
	q.EnqueueBacklog("batch-aggregate", fpList(300), 400)

	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := q.Drain(drainCtx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	deadline := time.After(5 * time.Second)
	var completes, progresses int
	var completion ProgressEvent

	for {
		select {
		case ev := <-events:
			switch ev.Phase {
			case "complete":
				completes++
				completion = ev
			case "progress":
				progresses++
			}

			if completes > 1 {
				t.Fatalf("received %d completion events, want exactly 1", completes)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the batch completion event")
		}

		if completes == 1 {
			break
		}
	}

	if progresses > 1 {
		// 300 fast tasks finish inside the 10s cadence window: at most
		// ONE throttled intermediate record may appear (and with this
		// tester usually zero). The old per-task model would have
		// emitted 300.
		t.Fatalf("progress records = %d, want <= 1 (bounded emission)", progresses)
	}

	if completion.Passed != 300 || completion.Completed != 300 || completion.Planned != 300 {
		t.Fatalf("completion = passed %d / completed %d / planned %d, want 300/300/300",
			completion.Passed, completion.Completed, completion.Planned)
	}
}

// TestBacklogBatchPlannedGrowsWithAdmission pins the planned counter:
// deferred candidates join the plan as they materialize, so the
// completion event can only fire after the WHOLE plan ran.
func TestBacklogBatchPlannedGrowsWithAdmission(t *testing.T) {
	cfg := Config{
		Concurrency:    0,
		Timeout:        time.Second,
		MaxAttempts:    1,
		AdmissionBatch: 10,
		AdmissionFloor: 30,
	}

	q := New(&stubTester{}, cfg)

	q.OpenBatch("batch-plan")
	accepted, materialized := q.EnqueueBacklog("batch-plan", fpList(40), 400)

	if accepted != 40 || materialized != 10 {
		t.Fatalf("accepted=%d materialized=%d, want 40/10", accepted, materialized)
	}

	// Two admissions later the plan must cover 30 candidates.
	q.admit()
	q.admit()

	q.mu.Lock()
	planned := q.batch.planned
	q.mu.Unlock()

	if planned != 30 {
		t.Fatalf("planned = %d, want 30 (10 + 2 admissions x 10)", planned)
	}

	// The completion event must NOT have fired while candidates remain.
	q.mu.Lock()
	finished := q.batch.finished
	backlog := len(q.backlog)
	q.mu.Unlock()

	if finished {
		t.Fatal("batch finished while backlog still had deferred candidates")
	}

	if backlog != 10 {
		t.Fatalf("backlog = %d, want 10", backlog)
	}
}
