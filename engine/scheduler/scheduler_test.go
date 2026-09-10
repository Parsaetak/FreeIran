package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerRunsCycles(t *testing.T) {
	var count atomic.Int64

	s := New(Options{
		Interval:   20 * time.Millisecond,
		RunOnStart: true,
	}, func(ctx context.Context) error {
		count.Add(1)

		return nil
	})

	s.Start(context.Background())
	defer s.Stop()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if count.Load() >= 3 {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("expected >= 3 cycles, got %d", count.Load())
}

func TestSchedulerSkipIfBusy(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	var calls atomic.Int64

	s := New(Options{Interval: 10 * time.Millisecond},
		func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}

			return nil
		})

	s.Start(context.Background())
	defer s.Stop()

	<-started

	// Wake repeatedly while the first cycle is still running; none
	// may start until it finishes.
	s.Wake()
	s.Wake()
	s.Wake()

	if s.Status().Busy != true {
		t.Fatal("scheduler should report busy")
	}

	close(release)

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("expected follow-up cycle, got %d calls", calls.Load())
}

func TestSchedulerStopWaitsForCycle(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	s := New(Options{Interval: time.Hour, RunOnStart: true},
		func(ctx context.Context) error {
			close(started)
			<-release

			return nil
		})

	s.Start(context.Background())

	<-started

	stopped := make(chan struct{})

	go func() {
		s.Stop()

		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while cycle still running")

	default:
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after cycle finished")
	}
}

func TestSchedulerCancellationPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	errInside := make(chan error, 1)

	s := New(Options{Interval: time.Hour, RunOnStart: true},
		func(ctx context.Context) error {
			<-ctx.Done()

			errInside <- ctx.Err()

			return ctx.Err()
		})

	s.Start(ctx)

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-errInside:
		return

	case <-time.After(2 * time.Second):
		t.Fatal("cycle never observed cancellation")
	}
}

func TestSchedulerWakeRunsImmediately(t *testing.T) {
	var count atomic.Int64

	s := New(Options{Interval: time.Hour}, func(ctx context.Context) error {
		count.Add(1)

		return nil
	})

	s.Start(context.Background())
	defer s.Stop()

	s.Wake()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if count.Load() >= 1 {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("wake did not trigger a cycle")
}

func TestSchedulerRecordsErrors(t *testing.T) {
	s := New(Options{Interval: 10 * time.Millisecond, RunOnStart: true},
		func(ctx context.Context) error {
			return context.DeadlineExceeded
		})

	s.Start(context.Background())
	defer s.Stop()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if s.Status().LastErr != "" {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("error not recorded")
}

func TestConcurrentWakeSafe(t *testing.T) {
	s := New(Options{Interval: 50 * time.Millisecond},
		func(ctx context.Context) error {
			return nil
		})

	s.Start(context.Background())
	defer s.Stop()

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := 0; j < 10; j++ {
				s.Wake()
			}
		}()
	}

	wg.Wait()
}
