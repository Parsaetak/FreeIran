// emitter.go implements the bounded, nonblocking delivery boundary
// for UI runtimes (v0.9.9): the composition root bridges authoritative
// snapshot streams into the UI event system through this emitter
// instead of emitting directly on the publisher's delivery goroutine.
//
// Problem it removes: a slow or stalled UI runtime (webview event
// pipeline busy) used to stall the publisher's single delivery
// goroutine inside the emit call, which filled the publisher's pending
// queue and made the overflow valve work overtime. With the emitter
// between the two:
//
//		publisher delivery goroutine        emitter pump goroutine
//		      drain() ── Submit (nonblocking) ──► bounded queue ──► UI emit
//
//	  - Submit NEVER blocks and NEVER allocates beyond the bound;
//	  - saturation coalesces to the newest snapshot: snapshots are
//	    FULL-STATE views, so the newest one supersedes all pending
//	    older ones and a resumed UI converges immediately (no engine
//	    state information is lost — the authoritative state machine
//	    keeps publishing);
//	  - Stop drains the newest pending snapshot into the UI and joins
//	    the pump, so no emit can run after Stop returns (same contract
//	    as Publisher.Stop — the composition root stops emitters before
//	    the UI runtime is destroyed);
//	  - publish order is preserved for everything that fits; only
//	    saturation coalesces, never reorders.
package statepub

import "sync"

// emitterQueue bounds one UI delivery boundary. UI event systems
// consume in microseconds when healthy; 32 outstanding snapshots
// imply a stalled webview, which is exactly the case this bound
// exists for.
const emitterQueue = 32

// BoundedEmitter forwards snapshots to a UI emit function from its
// own goroutine through a bounded, coalescing queue. The zero value
// is unusable; use NewBoundedEmitter.
type BoundedEmitter[T any] struct {
	emit func(T)

	mu      sync.Mutex
	queue   []T
	stopped bool

	signal chan struct{}
	quit   chan struct{}
	done   chan struct{}
}

// NewBoundedEmitter starts the delivery pump. emit must be safe to
// call from a non-main goroutine (Wails Event.Emit is).
func NewBoundedEmitter[T any](name string, emit func(T)) *BoundedEmitter[T] {
	e := &BoundedEmitter[T]{
		emit:   emit,
		signal: make(chan struct{}, 1),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}

	go e.loop()

	return e
}

// Submit hands one snapshot to the UI boundary without ever blocking
// the caller. Saturation keeps the NEWEST snapshot (full-state
// semantics: newer supersedes older — see the package doc).
func (e *BoundedEmitter[T]) Submit(snapshot T) {
	e.mu.Lock()

	if e.stopped {
		e.mu.Unlock()

		return
	}

	if len(e.queue) >= emitterQueue {
		// Coalesce: drop the OLDEST pending snapshot — for full-state
		// views the newest is the authoritative one and the queue
		// stays bounded without unboundedly growing.
		copy(e.queue, e.queue[1:])
		e.queue[len(e.queue)-1] = snapshot
	} else {
		e.queue = append(e.queue, snapshot)
	}

	select {
	case e.signal <- struct{}{}:
	default:
	}

	e.mu.Unlock()
}

// Stop synchronously drains the newest pending snapshot into the UI
// and joins the pump. Idempotent; after Stop returns no emit can run
// again and Submit is a safe no-op.
func (e *BoundedEmitter[T]) Stop() {
	e.mu.Lock()

	if e.stopped {
		e.mu.Unlock()

		<-e.done // already stopping/stopped: just join

		return
	}

	e.stopped = true
	close(e.quit)

	e.mu.Unlock()

	<-e.done
}

// loop is the single pump goroutine.
func (e *BoundedEmitter[T]) loop() {
	defer close(e.done)

	for {
		select {
		case <-e.signal:
			e.drain()

		case <-e.quit:
			// Final drain: the newest pending snapshot reaches the UI
			// (terminal-state convergence guarantee).
			e.drain()

			return
		}
	}
}

// drain delivers the pending queue, newest last, outside all locks.
func (e *BoundedEmitter[T]) drain() {
	for {
		e.mu.Lock()

		if len(e.queue) == 0 {
			e.mu.Unlock()

			return
		}

		snapshot := e.queue[0]
		e.queue = e.queue[1:]

		e.mu.Unlock()

		e.emit(snapshot)
	}
}
