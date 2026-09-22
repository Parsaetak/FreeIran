package singleflight

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// v0.9.14 CI-hardening stress coverage
//
// The implementation moved from a WaitGroup + per-waiter helper
// goroutine to a per-call completion channel. Semantics that MUST be
// preserved (and are pinned here): shared completion, per-caller
// cancellation without cancelling the shared work, error propagation,
// identical results for every waiter, and no goroutine accumulation
// from abandoned (cancelled) waiters.
// ---------------------------------------------------------------------------

// TestDoAbandonedWaitersLeaveNoGoroutines proves the channel-based
// wait leaves nothing behind: many callers time out (abandon) while
// the shared execution is slow; after the execution completes and the
// runtime settles, the goroutine count returns to baseline.
func TestDoAbandonedWaitersLeaveNoGoroutines(t *testing.T) {
	if testing.Short() {
		t.Skip("goroutine-settling test skipped in -short mode")
	}

	g := &Group[int]{}

	release := make(chan struct{})

	var executions atomic.Int32

	execTimeout := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	}

	fn := func(context.Context) (int, error) {
		executions.Add(1)
		<-release

		return 1, nil
	}

	before := runtime.NumGoroutine()

	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()

			// Every waiter abandons: the shared work outlives them all.
			_, _ = Do(g, ctx, "abandon", execTimeout, fn)
		}()
	}

	wg.Wait()

	// The shared execution is still running; exactly one execution.
	if got := executions.Load(); got != 1 {
		t.Fatalf("executions = %d, want 1 (abandoned callers must not restart work)", got)
	}

	close(release)

	// Let the executor finish and the runtime reap the goroutine.
	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	after := runtime.NumGoroutine()

	if after > before+2 {
		t.Fatalf("goroutines before=%d after=%d; abandoned waiters leaked goroutines", before, after)
	}
}

// TestDoWaitersShareIdenticalResults hammers the completion channel
// with high contention: 200 waiters on one key all receive the exact
// same result once the single execution finishes.
func TestDoWaitersShareIdenticalResults(t *testing.T) {
	g := &Group[string]{}

	started := make(chan struct{})

	execTimeout := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	}

	fn := func(context.Context) (string, error) {
		close(started)

		time.Sleep(50 * time.Millisecond)

		return "shared-value", nil
	}

	var wg sync.WaitGroup

	errs := make([]error, 200)
	vals := make([]string, 200)

	for i := 0; i < 200; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			v, err := Do(g, context.Background(), "contended", execTimeout, fn)
			vals[i], errs[i] = v, err
		}(i)
	}

	<-started
	wg.Wait()

	for i := range vals {
		if errs[i] != nil {
			t.Fatalf("waiter %d: %v", i, errs[i])
		}

		if vals[i] != "shared-value" {
			t.Fatalf("waiter %d: value %q, want the shared value", i, vals[i])
		}
	}
}

// TestDoSequentialCallsAfterCompletionExecuteFreshly proves the key
// is retired before waiters are released: a call after completion
// starts a NEW execution instead of replaying the old result.
func TestDoSequentialCallsAfterCompletionExecuteFreshly(t *testing.T) {
	g := &Group[int]{}

	execTimeout := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	}

	var executions atomic.Int32

	fn := func(context.Context) (int, error) {
		return int(executions.Add(1)), nil
	}

	for i := 1; i <= 3; i++ {
		v, err := Do(g, context.Background(), "seq", execTimeout, fn)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}

		if v != i {
			t.Fatalf("call %d returned %d; each sequential call must execute freshly", i, v)
		}
	}
}

// TestDoFailedExecutionIsNotCached pins error propagation: waiters of
// a failed execution receive the error, and the NEXT call starts a
// fresh execution (failures are never sticky).
func TestDoFailedExecutionIsNotCached(t *testing.T) {
	g := &Group[int]{}

	execTimeout := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	}

	boom := errors.New("boom")

	fnErr := func(context.Context) (int, error) { return 0, boom }

	_, err := Do(g, context.Background(), "fail", execTimeout, fnErr)
	if !errors.Is(err, boom) {
		t.Fatalf("first call error = %v, want boom", err)
	}

	v, err := Do(g, context.Background(), "fail", execTimeout, func(context.Context) (int, error) {
		return 7, nil
	})

	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if v != 7 {
		t.Fatalf("second call = %d, want a fresh execution (7)", v)
	}
}

// TestDoConcurrentMixedKeys stress-races many distinct keys so the
// group map, the retire-before-release ordering and the per-call
// channels are exercised under -race. Waiter slots 3/7/11/… carry a
// 1ms budget against a 5ms execution: they exercise the abandoned-
// waiter branch (own context error, shared work untouched) while the
// remaining waiters receive the shared result. Both branches are
// ASSERTED to have run — the waiters and the keys are passed into the
// goroutines explicitly so the cancellation path cannot be optimized
// away by a capture mistake.
func TestDoConcurrentMixedKeys(t *testing.T) {
	g := &Group[int]{}

	execTimeout := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	}

	const numKeys = 20
	const waitersPerKey = 10

	var wg sync.WaitGroup

	// executions[key] counts how often the shared work ran: exactly
	// once per key, no matter how many waiters abandoned it.
	executions := make([]atomic.Int32, numKeys)

	// cancelled/succeeded count the observed waiter outcomes across
	// ALL keys — the test fails if either branch never ran.
	var cancelled, succeeded atomic.Int32

	for key := 0; key < numKeys; key++ {
		for waiter := 0; waiter < waitersPerKey; waiter++ {
			wg.Add(1)

			go func(key, waiter int) {
				defer wg.Done()

				ctx := context.Background()

				if waiter%4 == 3 {
					var cancel context.CancelFunc

					ctx, cancel = context.WithTimeout(ctx, time.Millisecond)
					defer cancel()
				}

				v, err := Do(g, ctx, keys[key], execTimeout, func(context.Context) (int, error) {
					executions[key].Add(1)

					time.Sleep(5 * time.Millisecond)

					return key, nil
				})

				if err != nil {
					// An abandoned waiter reports ITS OWN context
					// error — never the shared result, never a
					// foreign failure.
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("key %d waiter %d: unexpected error %v", key, waiter, err)

						return
					}

					cancelled.Add(1)

					return
				}

				succeeded.Add(1)

				if v != key {
					t.Errorf("key %d: value %d leaked across keys", key, v)
				}
			}(key, waiter)
		}
	}

	wg.Wait()

	// The cancellation branch must genuinely have been exercised:
	// 5 keys × waiters 3 and 7 per 10-waiter wave hold a 1ms budget
	// against a 5ms execution.
	if cancelled.Load() == 0 {
		t.Fatal("no waiter was ever cancelled; the abandoned-waiter branch is not covered")
	}

	if succeeded.Load() == 0 {
		t.Fatal("no waiter received the shared result; coverage is one-sided")
	}

	// Abandoned waiters must not restart shared work: one execution
	// per key, exactly.
	for key := range executions {
		if got := executions[key].Load(); got != 1 {
			t.Errorf("key %d: executions = %d, want 1 (abandoned waiters must not duplicate work)", key, got)
		}
	}
}

// keys for TestDoConcurrentMixedKeys (package-level so the closure
// captures nothing per iteration).
var keys = func() [20]string {
	var k [20]string

	for i := range k {
		k[i] = "key-" + strconv.Itoa(i)
	}

	return k
}()
