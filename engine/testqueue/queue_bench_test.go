package testqueue

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkQueueEnqueue measures enqueue throughput at three scales.
// The queue has no workers (Concurrency=0) so tasks stay queued,
// isolating the enqueue + heap-push cost.
func BenchmarkQueueEnqueue(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				q := New(NoopTester{}, Config{
					Concurrency:  0,
					MaxQueueSize: 200000,
					Timeout:      5 * time.Second,
					MaxAttempts:  1,
				})
				q.Start(context.Background())
				b.StartTimer()

				for j := 0; j < n; j++ {
					q.Enqueue(fmt.Sprintf("fp-%d", j), "vless", nil, 100, "src", EnqueueDefault)
				}

				b.StopTimer()
				q.Stop()
			}
		})
	}
}

// BenchmarkQueueDequeue measures dequeue throughput with a single
// worker consuming tasks as fast as possible.
func BenchmarkQueueDequeue(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				q := New(NoopTester{}, Config{
					Concurrency:  1,
					MaxQueueSize: 200000,
					Timeout:      1 * time.Hour, // prevent test completion
					MaxAttempts:  1,
				})
				ctx, cancel := context.WithCancel(context.Background())
				q.Start(ctx)

				for j := 0; j < n; j++ {
					q.Enqueue(fmt.Sprintf("fp-%d", j), "vless", nil, 100, "src", EnqueueDefault)
				}

				var dequeued atomic.Int64
				done := make(chan struct{})
				go func() {
					for {
						_, err := q.Dequeue(ctx)
						if err != nil {
							close(done)
							return
						}
						dequeued.Add(1)
						// Simulate instant completion by finishing the task.
					}
				}()

				b.StartTimer()
				// Wait for all tasks to be dequeued.
				for dequeued.Load() < int64(n) {
					time.Sleep(time.Microsecond)
				}
				b.StopTimer()

				cancel()
				q.Stop()
				<-done
			}
		})
	}
}

// BenchmarkQueueCancelBySource measures cancellation throughput.
func BenchmarkQueueCancelBySource(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				q := New(NoopTester{}, Config{
					Concurrency:  0,
					MaxQueueSize: 200000,
					Timeout:      5 * time.Second,
					MaxAttempts:  1,
				})
				q.Start(context.Background())

				for j := 0; j < n; j++ {
					q.Enqueue(fmt.Sprintf("fp-%d", j), "vless", nil, 100, "src-A", EnqueueDefault)
				}
				b.StartTimer()

				q.CancelBySource("src-A")

				b.StopTimer()
				q.Stop()
			}
		})
	}
}

// BenchmarkQueueTaskSize estimates the per-task memory overhead by
// allocating N tasks and measuring bytes/task via b.ReportAllocs.
func BenchmarkQueueTaskSize(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		t := &Task{
			ID:          int64(i),
			Fingerprint: fmt.Sprintf("fp-%d", i),
			Protocol:    "vless",
			Backends:    []string{"xray"},
			Priority:    100,
			Source:      "src",
			Attempt:     1,
			MaxAttempts: 2,
			CreatedAt:   time.Now().UTC(),
			Deadline:    time.Now().Add(30 * time.Second),
			State:       StateQueued,
			heapIdx:     -1,
		}
		_ = t
	}
}
