// Package statepub implements the deduplicated, coalescing state
// publisher used to push authoritative state snapshots to the UI.
//
// v0.9.8.7 replaces the 2-second ticker broadcasts with real
// transition-driven publishing:
//
//	REAL STATE TRANSITION
//	        ↓
//	DEDUPLICATED STATE PUBLISHER (semantic equality check)
//	        ↓
//	OPTIONAL SHORT COALESCING WINDOW (burst damping)
//	        ↓
//	EMIT CALLBACK (the composition root forwards to the UI runtime)
//
// Publish is called from the authoritative transition path the moment
// meaningful state changes. Identical snapshots are dropped (semantic
// equality), so re-publishing an unchanged state costs nothing and the
// UI never re-renders on duplicate payloads. A burst of transitions
// within the coalescing window collapses into ONE emission of the
// newest snapshot — ordered, never re-ordered, never delayed beyond
// the short window.
//
// Lifecycle: Stop is synchronous and idempotent. After Stop returns,
// no further emit callback can run and no internal goroutine remains
// — no publisher may outlive App.Shutdown() or fire into a destroyed
// UI runtime. Publish after Stop is a safe no-op.
package statepub

import (
	"sync"
	"time"
)

// DefaultCoalesce is the default coalescing window: long enough to
// collapse a same-tick burst of transitions into one event, far below
// anything a user can perceive.
const DefaultCoalesce = 25 * time.Millisecond

// Publisher deduplicates and coalesces state snapshots of type T and
// delivers each real change to the emit callback on its own
// goroutine (never on the caller's, never while the source holds a
// lock).
type Publisher[T any] struct {
	name     string
	equal    func(a, b T) bool
	emit     func(T)
	coalesce time.Duration

	// mu guards the publish slot and the stopped flag.
	mu      sync.Mutex
	pending T
	dirty   bool
	last    T
	hasLast bool
	stopped bool

	// signal wakes the delivery goroutine (buffered, capacity 1: a
	// second Publish while one signal is pending needs no second
	// signal — the goroutine re-reads the newest pending slot).
	signal chan struct{}
	done   chan struct{}
}

// New creates a publisher. equal is the semantic-equality predicate
// (nil = every snapshot is treated as changed); emit is the delivery
// callback (nil means snapshots are dropped — useful in tests);
// coalesce bounds the burst-collapsing window (<= 0 selects
// DefaultCoalesce). The delivery goroutine starts immediately.
func New[T any](
	name string,
	emit func(T),
	equal func(a, b T) bool,
	coalesce time.Duration,
) *Publisher[T] {
	if coalesce <= 0 {
		coalesce = DefaultCoalesce
	}

	p := &Publisher[T]{
		name:     name,
		equal:    equal,
		emit:     emit,
		coalesce: coalesce,
		signal:   make(chan struct{}, 1),
		done:     make(chan struct{}),
	}

	go p.loop()

	return p
}

// Publish submits a snapshot. Semantics:
//
//   - stopped → no-op;
//   - semantically equal to the newest submitted (not necessarily
//     already emitted) snapshot → dropped, no signal;
//   - otherwise it becomes the newest pending snapshot and the
//     delivery goroutine is woken.
//
// Publish never blocks on the consumer: the pending slot holds the
// newest snapshot and older pending snapshots are superseded in
// place, so a burst cannot queue up (memory-bounded by construction).
// A monotonic revision field on the snapshot type composes with
// equal: same state + same revision = no event, real change = event.
func (p *Publisher[T]) Publish(snapshot T) {
	p.mu.Lock()

	if p.stopped {
		p.mu.Unlock()

		return
	}

	if p.hasLast && p.equal != nil && p.equal(p.last, snapshot) {
		p.mu.Unlock()

		return
	}

	p.last = snapshot
	p.hasLast = true
	p.pending = snapshot
	p.dirty = true

	// The wake-up signal is sent UNDER the mutex: Stop closes the
	// channel under the same mutex, so a send can never race a close
	// (sending on a closed channel would panic). The send is
	// non-blocking, so holding the mutex here cannot deadlock.
	select {
	case p.signal <- struct{}{}:
	default:
	}

	p.mu.Unlock()
}

// Stop terminates the publisher synchronously and idempotently. After
// the first Stop returns: the delivery goroutine has exited, no emit
// callback can run again, and every later Publish is a no-op. This is
// the guarantee that lets the composition root stop the publisher
// BEFORE the UI runtime is destroyed — no callback ever fires into a
// dead event target.
func (p *Publisher[T]) Stop() {
	p.mu.Lock()

	if p.stopped {
		p.mu.Unlock()

		return
	}

	// stopped is set and the signal channel is closed under the mutex
	// so no in-flight Publish send can race the close (see Publish).
	p.stopped = true
	close(p.signal)

	p.mu.Unlock()

	<-p.done
}

// Name returns the publisher's diagnostic name.
func (p *Publisher[T]) Name() string {
	return p.name
}

// loop is the single delivery goroutine. It drains the newest pending
// snapshot, applies the coalescing window, and emits. Emission always
// happens outside the mutex so a slow consumer can never block a
// state transition.
func (p *Publisher[T]) loop() {
	defer close(p.done)

	for range p.signal {
		// Short coalescing window: transitions that arrive while we
		// sleep collapse into the single pending slot (Publish keeps
		// only the newest). The window is bounded and tiny — it damps
		// bursts without introducing a user-visible delay.
		if p.coalesce > 0 {
			time.Sleep(p.coalesce)
		}

		p.mu.Lock()

		if p.stopped {
			p.mu.Unlock()

			return
		}

		if !p.dirty {
			// Superseded: Stop raced a signal, or the pending slot was
			// already consumed. Nothing to emit.
			p.mu.Unlock()

			continue
		}

		snapshot := p.pending
		p.dirty = false

		p.mu.Unlock()

		// Emit OUTSIDE the lock. equal-based dedup already happened in
		// Publish; the emitted snapshot is the newest real change.
		if p.emit != nil {
			p.emit(snapshot)
		}
	}
}
