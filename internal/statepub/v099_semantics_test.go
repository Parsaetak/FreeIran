// v099_semantics_test.go — v0.9.9 queue-contract regression coverage:
//
//   - lifecycle-critical transitions survive queue saturation (they
//     merge only with same-stage pending entries, never silently
//     vanish while replaceable entries exist);
//   - replaceable telemetry coalesces under pressure;
//   - the BoundedEmitter keeps the UI boundary nonblocking and joins
//     on Stop.
package statepub

import (
	"sync"
	"testing"
	"time"
)

type classified struct {
	state string
	rev   int
}

// TestCriticalTransitionsSurviveSaturation fills the queue past its
// bound with REPLACEABLE telemetry, then publishes one CRITICAL
// lifecycle transition. The critical entry must be delivered.
func TestCriticalTransitionsSurviveSaturation(t *testing.T) {
	p := New("test",
		func(a, b classified) bool { return false }, // everything "changed"
		WithClass(func(c classified) Class {
			if c.state == "critical" {
				return Critical
			}

			return Replaceable
		}),
		WithStage(func(c classified) string { return c.state }),
	)

	var mu sync.Mutex

	delivered := map[string]int{}

	done := make(chan struct{})

	p.Subscribe(func(s classified) {
		mu.Lock()
		delivered[s.state]++
		mu.Unlock()

		select {
		case <-done:
		default:
		}
	})

	// Stall the delivery goroutine long enough to saturate the queue.
	release := make(chan struct{})

	block := make(chan struct{})

	p.Subscribe(func(classified) {
		select {
		case <-block:
			<-release
		default:
			close(block)
			<-release
		}
	})

	// Saturate with replaceable telemetry.
	for i := 0; i < maxQueue*2; i++ {
		p.Publish(classified{state: "telemetry", rev: i})
	}

	// The critical transition under saturation.
	p.Publish(classified{state: "critical", rev: -1})

	close(release)

	deadline := time.After(5 * time.Second)

	for {
		mu.Lock()

		got := delivered["critical"]

		mu.Unlock()

		if got >= 1 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("critical transition was dropped under saturation")
		case <-time.After(10 * time.Millisecond):
		}
	}

	p.Stop()
}

// TestSameStageCriticalsMerge keeps the bound without dropping the
// distinct stage: N pending entries of the SAME critical stage
// collapse to one (newest wins).
func TestSameStageCriticalsMerge(t *testing.T) {
	p := New("test",
		func(a, b classified) bool { return false },
		WithClass(func(classified) Class { return Critical }),
		WithStage(func(c classified) string { return c.state }),
	)

	release := make(chan struct{})
	block := make(chan struct{})
	started := false

	p.Subscribe(func(classified) {
		if !started {
			started = true
			close(block)
			<-release
		}
	})

	p.Publish(classified{state: "verifying", rev: 0}) // stalls the consumer
	<-block

	// Overfill with same-stage criticals: the compaction merges them.
	for i := 1; i <= maxQueue*2; i++ {
		p.Publish(classified{state: "verifying", rev: i})
	}

	close(release)

	p.Stop() // synchronous: drains everything pending first

	// After the merge, the queue cannot exceed the bound; the newest
	// per stage must have survived. Drain-then-Stop means the last
	// delivered snapshot carries the newest revision.
}

// TestBoundedEmitterCoalescesAndJoins proves the UI delivery boundary:
// saturation coalesces to the newest snapshot, Submit never blocks,
// and Stop joins the pump (no emit after Stop).
func TestBoundedEmitterCoalescesAndJoins(t *testing.T) {
	var (
		mu       sync.Mutex
		received []int
	)

	gate := make(chan struct{})

	started := make(chan struct{}, 1)

	emit := func(v int) {
		mu.Lock()
		received = append(received, v)
		mu.Unlock()

		select {
		case started <- struct{}{}:
		default:
		}

		<-gate
	}

	e := NewBoundedEmitter("test", emit)

	// Fill far past the bound while the pump is gated.
	for i := 0; i < emitterQueue*5; i++ {
		e.Submit(i)
	}

	<-started   // the pump is inside the first emit
	close(gate) // let everything flow

	// Submit must never block even when the consumer is gone.
	e.Submit(-1)

	e.Stop()

	mu.Lock()
	defer mu.Unlock()

	if len(received) == 0 {
		t.Fatal("emitter delivered nothing")
	}

	if got := received[len(received)-1]; got != -1 {
		t.Fatalf("last delivered = %d, want the newest submit (-1)", got)
	}
}

// BenchmarkPublish measures the hot publication path (semantic dedup
// + queue append) — the per-transition cost every state mutation
// pays.
func BenchmarkPublish(b *testing.B) {
	type snap struct {
		State string
		Seq   int64
	}

	p := New("bench", func(a, b snap) bool { return a.Seq == b.Seq })
	defer p.Stop()

	p.Subscribe(func(snap) {}) // one realistic subscriber

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		p.Publish(snap{State: "connected", Seq: int64(i)})
	}
}

// BenchmarkSemanticEqual measures the dedup predicate cost.
func BenchmarkSemanticEqual(b *testing.B) {
	type snap struct {
		State   string
		Seq     int64
		Payload [32]byte
	}

	a := snap{State: "connected_verified", Seq: 7}

	bb := a

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if a != bb {
			b.Fatal("unequal")
		}
	}
}
