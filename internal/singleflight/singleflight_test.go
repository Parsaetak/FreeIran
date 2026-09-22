package singleflight

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoDeduplicatesConcurrentCallers(t *testing.T) {
	var (
		g       Group[string]
		execCnt int64
	)

	const callers = 16

	var wg sync.WaitGroup

	results := make([]string, callers)

	key := "release:latest"

	for i := range callers {
		wg.Add(1)

		go func(slot int) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			v, err := Do(&g, ctx, key,
				func() (context.Context, context.CancelFunc) {
					return context.WithTimeout(context.Background(), time.Second)
				},
				func(ctx context.Context) (string, error) {
					atomic.AddInt64(&execCnt, 1)

					time.Sleep(20 * time.Millisecond)

					return "shared-result", nil
				})
			if err != nil {
				t.Errorf("caller %d: %v", slot, err)

				return
			}

			results[slot] = v
		}(i)
	}

	wg.Wait()

	if got := atomic.LoadInt64(&execCnt); got != 1 {
		t.Fatalf("executions = %d, want 1", got)
	}

	for i, r := range results {
		if r != "shared-result" {
			t.Fatalf("result[%d] = %q, want shared result", i, r)
		}
	}
}

func TestDoCancelledCallerDoesNotCancelSharedWork(t *testing.T) {
	var g Group[int]

	sharedCtx, cancelShared := context.WithCancel(context.Background())
	defer cancelShared()

	execStarted := make(chan struct{})
	execDone := make(chan struct{})

	// First caller: starts the shared execution with a detached
	// context that outlives its own cancellation.
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		_, _ = Do(&g, ctx, "shared",
			func() (context.Context, context.CancelFunc) {
				return sharedCtx, func() {}
			},
			func(ctx context.Context) (int, error) {
				close(execStarted)
				time.Sleep(50 * time.Millisecond)
				close(execDone)

				return 42, nil
			})
	}()

	<-execStarted

	// Waiter with an already-cancelled context: returns its own error
	// immediately; the shared execution completes for future callers.
	cancelledCtx, cancelWaiter := context.WithCancel(context.Background())
	cancelWaiter()

	if _, err := Do(&g, cancelledCtx, "shared",
		func() (context.Context, context.CancelFunc) {
			return context.Background(), func() {}
		},
		func(ctx context.Context) (int, error) { return 0, nil }); err == nil {
		t.Fatal("cancelled waiter should return its own ctx error")
	}

	select {
	case <-execDone:
		// The shared execution was NOT cancelled by the waiter.
	case <-time.After(2 * time.Second):
		t.Fatal("shared execution was cancelled by a waiter — contract violated")
	}

	// A later caller still gets the shared result once in flight
	// completes... and a NEW call after completion re-executes.
	v, err := Do(&g, context.Background(), "shared",
		func() (context.Context, context.CancelFunc) {
			return context.Background(), func() {}
		},
		func(ctx context.Context) (int, error) {
			return 43, nil
		})
	if err != nil || v != 43 {
		t.Fatalf("post-completion call = %d, %v; want fresh execution", v, err)
	}
}

func TestDoPropagatesError(t *testing.T) {
	var g Group[struct{}]

	_, err := Do(&g, context.Background(), "boom",
		func() (context.Context, context.CancelFunc) {
			return context.Background(), func() {}
		},
		func(ctx context.Context) (struct{}, error) {
			return struct{}{}, errTest
		})
	if err != errTest {
		t.Fatalf("err = %v, want errTest", err)
	}
}

var errTest = &testError{}

type testError struct{}

func (*testError) Error() string { return "test failure" }
