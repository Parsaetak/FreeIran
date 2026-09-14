package cleanup

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunExecutesTasksInOrder verifies ordered execution, byte
// accounting and per-task results.
func TestRunExecutesTasksInOrder(t *testing.T) {
	m := New(Options{MinInterval: time.Nanosecond, MaxDuration: 5 * time.Second})

	var order []string

	m.Register(Task{
		Name: "a",
		Run: func(context.Context) (int64, int64, error) {
			order = append(order, "a")

			return 100, 2, nil
		},
	})

	m.Register(Task{
		Name: "b",
		Run: func(context.Context) (int64, int64, error) {
			order = append(order, "b")

			return 50, 1, nil
		},
	})

	result, ran := m.Run(context.Background())
	if !ran {
		t.Fatal("first Run reported not-run")
	}

	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("execution order = %v, want [a b]", order)
	}

	if result.Bytes != 150 {
		t.Fatalf("total bytes = %d, want 150", result.Bytes)
	}

	if len(result.Tasks) != 2 {
		t.Fatalf("task results = %d, want 2", len(result.Tasks))
	}

	if m.TotalReclaimed() != 150 || m.Passes() != 1 {
		t.Fatalf("cumulative counters: bytes=%d passes=%d", m.TotalReclaimed(), m.Passes())
	}

	if last, ok := m.LastResult(); !ok || last.Bytes != 150 {
		t.Fatalf("LastResult = %+v ok=%v", last, ok)
	}
}

// TestRunRateLimits verifies MinInterval suppresses stampedes of
// pressure-triggered passes.
func TestRunRateLimits(t *testing.T) {
	m := New(Options{MinInterval: time.Hour})

	m.Register(Task{
		Name: "x",
		Run: func(context.Context) (int64, int64, error) {
			return 1, 1, nil
		},
	})

	if _, ran := m.Run(context.Background()); !ran {
		t.Fatal("first run should execute")
	}

	if _, ran := m.Run(context.Background()); ran {
		t.Fatal("second run inside MinInterval should be suppressed")
	}
}

// TestRunSkipsTasksWithoutWork verifies the skipped flag and that a
// failing task does not abort the pass.
func TestRunSkipsTasksWithoutWork(t *testing.T) {
	m := New(Options{MinInterval: time.Nanosecond})

	m.Register(Task{
		Name: "nothing",
		Run: func(context.Context) (int64, int64, error) {
			return 0, 0, nil
		},
	})

	m.Register(Task{
		Name: "boom",
		Run: func(context.Context) (int64, int64, error) {
			return 0, 0, errors.New("boom")
		},
	})

	m.Register(Task{
		Name: "works",
		Run: func(context.Context) (int64, int64, error) {
			return 10, 1, nil
		},
	})

	result, ran := m.Run(context.Background())
	if !ran {
		t.Fatal("pass should run")
	}

	if result.Tasks[0].Skipped != true {
		t.Fatal("empty task should be marked skipped")
	}

	if result.Tasks[1].Error == "" {
		t.Fatal("failing task should carry its error")
	}

	if result.Tasks[2].Skipped {
		t.Fatal("working task should not be skipped")
	}

	if result.Bytes != 10 {
		t.Fatalf("bytes = %d, want 10 (failed task must not contribute)", result.Bytes)
	}
}

// TestRunCancellable verifies ctx cancellation stops the pass.
func TestRunCancellable(t *testing.T) {
	m := New(Options{MinInterval: time.Nanosecond, MaxDuration: 5 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())

	var executed atomic.Bool

	m.Register(Task{
		Name: "first",
		Run: func(context.Context) (int64, int64, error) {
			cancel() // cancel while running

			return 1, 1, nil
		},
	})

	m.Register(Task{
		Name: "second",
		Run: func(ctx context.Context) (int64, int64, error) {
			executed.Store(true)
			_ = ctx

			return 1, 1, nil
		},
	})

	result, ran := m.Run(ctx)
	if !ran {
		t.Fatal("pass should run")
	}

	if executed.Load() {
		t.Fatal("second task should not execute after cancellation")
	}

	if result.Bytes != 1 {
		t.Fatalf("bytes = %d, want 1", result.Bytes)
	}
}

// TestConcurrentRunIsSerialized verifies concurrent callers cannot
// stampede: exactly one pass executes.
func TestConcurrentRunIsSerialized(t *testing.T) {
	m := New(Options{MinInterval: time.Nanosecond})

	var runs atomic.Int64

	m.Register(Task{
		Name: "counted",
		Run: func(context.Context) (int64, int64, error) {
			runs.Add(1)
			time.Sleep(20 * time.Millisecond)

			return 1, 1, nil
		},
	})

	done := make(chan bool, 4)

	for i := 0; i < 4; i++ {
		go func() {
			_, ran := m.Run(context.Background())
			done <- ran
		}()
	}

	ranCount := 0

	for i := 0; i < 4; i++ {
		if <-done {
			ranCount++
		}
	}

	if ranCount != 1 {
		t.Fatalf("ran passes = %d, want 1", ranCount)
	}

	if runs.Load() != 1 {
		t.Fatalf("task executions = %d, want 1", runs.Load())
	}
}
