package testqueue

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTester is a controllable Tester for unit tests.
type fakeTester struct {
	calls    atomic.Int64
	delay    time.Duration
	outcomes map[string]Result // fingerprint → outcome
}

func (f *fakeTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return Result{LastError: "cancelled"}, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if r, ok := f.outcomes[fp]; ok {
		return r, nil
	}
	return Result{Working: true, Latency: 50 * time.Millisecond, TestedAt: time.Now().UTC()}, nil
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

// TestCancelBySource verifies source-scoped cancellation.
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
