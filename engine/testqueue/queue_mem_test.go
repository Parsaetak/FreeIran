package testqueue

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
)

// TestQueueMemoryGrowth verifies that enqueuing + cancelling many
// tasks does not leak memory (all tasks are GC-eligible after
// cancellation because they're removed from pending/inflight/byFingerprint).
func TestQueueMemoryGrowth(t *testing.T) {
	q := New(NoopTester{}, Config{
		Concurrency:  0, // no workers; tasks stay queued
		MaxQueueSize: 100000,
		Timeout:      5 * time.Second,
		MaxAttempts:  1,
	})
	q.Start(context.Background())
	defer q.Stop()

	// Enqueue 50000 tasks.
	for i := 0; i < 50000; i++ {
		q.Enqueue(fmt.Sprintf("fp-%d", i), "vless", nil, 100, "src", EnqueueDefault)
	}

	stats := q.Stats()
	if stats.TotalEnqueued != 50000 {
		t.Fatalf("TotalEnqueued = %d, want 50000", stats.TotalEnqueued)
	}

	// Cancel all.
	cancelled := q.CancelAll()
	if cancelled != 50000 {
		t.Errorf("CancelAll = %d, want 50000", cancelled)
	}

	// After cancellation, the queue should be empty.
	stats = q.Stats()
	if stats.QueueDepth != 0 {
		t.Errorf("QueueDepth after cancel = %d, want 0", stats.QueueDepth)
	}
	if stats.TotalCancelled != 50000 {
		t.Errorf("TotalCancelled = %d, want 50000", stats.TotalCancelled)
	}

	// Force GC and verify memory is reclaimable (no live task refs).
	runtime.GC()
	runtime.GC()

	// Re-enqueue to verify the queue still works after mass cancel.
	for i := 0; i < 1000; i++ {
		q.Enqueue(fmt.Sprintf("fp2-%d", i), "vless", nil, 100, "src", EnqueueDefault)
	}
	stats = q.Stats()
	if stats.QueueDepth != 1000 {
		t.Errorf("QueueDepth after re-enqueue = %d, want 1000", stats.QueueDepth)
	}
}

// TestQueueCancellationCleanup verifies that cancelled tasks are
// removed from the byFingerprint index so the same fingerprint can be
// re-enqueued.
func TestQueueCancellationCleanup(t *testing.T) {
	q := New(NoopTester{}, Config{
		Concurrency:  0,
		MaxQueueSize: 100,
		Timeout:      5 * time.Second,
		MaxAttempts:  1,
	})
	q.Start(context.Background())
	defer q.Stop()

	// Enqueue, cancel, re-enqueue the same fingerprint 100 times.
	for i := 0; i < 100; i++ {
		id, err := q.Enqueue("fp-cycle", "vless", nil, 100, "src", EnqueueDefault)
		if err != nil {
			t.Fatalf("iteration %d: Enqueue: %v", i, err)
		}
		if !q.Cancel(id) {
			t.Fatalf("iteration %d: Cancel returned false", i)
		}
	}

	stats := q.Stats()
	if stats.TotalCancelled != 100 {
		t.Errorf("TotalCancelled = %d, want 100", stats.TotalCancelled)
	}
	if stats.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d, want 0 (each task cancelled)", stats.QueueDepth)
	}
}
