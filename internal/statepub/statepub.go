// Package statepub implements the single publisher/subscriber
// boundary that forwards authoritative state snapshots to the UI.
//
// Data flow (one boundary, no stacked dispatch layers):
//
//	AUTHORITATIVE MUTATION (source lock held)
//	        ↓  Publish(snapshot)   — semantic dedup, ordered queue
//	DELIVERY GOROUTINE             — zero artificial delay
//	        ↓
//	SUBSCRIBERS (composition root bridges to the UI runtime)
//
// Delivery contract:
//
//   - identical snapshots are suppressed (semantic equality against
//     the newest submitted snapshot — same state = no event, so
//     re-publishing an unchanged state costs nothing);
//   - every REAL change is delivered, in publication order, with no
//     artificial delay: connection lifecycle transitions
//     (selecting → preparing → starting_core → …) are user-visible
//     semantics and must never be silently dropped by a coalescing
//     window;
//   - memory is bounded AND lifecycle-critical transitions are never
//     silently dropped (v0.9.9): every snapshot carries a Class.
//     REPLACEABLE snapshots (high-frequency telemetry, superseded
//     intermediates) may be coalesced or dropped under queue
//     pressure; CRITICAL snapshots (real lifecycle transitions) are
//     only ever merged with a pending snapshot of the SAME stage
//     (newest wins — the distinct stages still arrive, in order).
//     Overflow admission compacts the pending queue in that order.
//     The absolute last resort — a queue consisting solely of
//     distinct critical stages beyond maxQueue — drops the oldest
//     entry to keep the bound honest; for a bounded state machine
//     (a handful of distinct stages per session) that case is
//     unreachable by construction. Without a classifier every
//     snapshot is Replaceable (the historical drop-oldest valve);
//   - Stop is synchronous and idempotent. Pending snapshots published
//     before Stop are drained and delivered FIRST — a terminal
//     shutdown state always reaches the subscribers — and after Stop
//     returns no callback can run again and no goroutine remains.
//     Publish after Stop is a safe no-op.
//
// The engine never imports the UI runtime; only the composition root
// registers subscribers.
package statepub

import (
	"sync"
)

// maxQueue bounds the pending transition queue. Real state
// transitions are milliseconds apart (process spawn, port wait,
// verification round-trip), so 256 outstanding snapshots imply a
// stalled consumer; the overflow valve then compacts the queue —
// coalescing same-stage entries and shedding replaceable ones —
// while keeping the newest transition and the delivery order of the
// rest.
const maxQueue = 256

// Class classifies a snapshot for the overflow admission policy.
type Class uint8

const (
	// Replaceable: the snapshot may be coalesced or dropped under
	// queue pressure (high-frequency telemetry, superseded
	// intermediates). Publishers without a classifier mark every
	// snapshot Replaceable.
	Replaceable Class = iota

	// Critical: a real lifecycle transition. Never silently dropped
	// while replaceable or same-stage pending entries exist (see the
	// package contract above).
	Critical
)

// queueItem is one pending snapshot plus its overflow classification.
type queueItem[T any] struct {
	snapshot T
	class    Class
	stage    string
}

// Option configures a Publisher.
type Option[T any] func(*publisherOptions[T])

type publisherOptions[T any] struct {
	classify func(T) Class
	stage    func(T) string
}

// WithClass installs the overflow classifier: snapshots for which fn
// returns Critical are lifecycle transitions the valve must preserve.
func WithClass[T any](fn func(T) Class) Option[T] {
	return func(o *publisherOptions[T]) { o.classify = fn }
}

// WithStage installs the stage function used to merge pending
// snapshots of the SAME lifecycle stage (newest wins). Optional:
// without it, no pending merging happens.
func WithStage[T any](fn func(T) string) Option[T] {
	return func(o *publisherOptions[T]) { o.stage = fn }
}

// Publisher delivers deduplicated, ordered snapshots of type T to its
// subscribers on a single internal goroutine — never on the
// publisher's, never while the source holds its lock.
type Publisher[T any] struct {
	name  string
	equal func(a, b T) bool

	subMu       sync.Mutex
	subscribers map[int]func(T)
	subSeq      int

	// mu guards the pending queue and the stopped flag. The queue is
	// FIFO: snapshots are delivered in publication order.
	mu      sync.Mutex
	queue   []queueItem[T]
	last    T // newest submitted snapshot (dedup reference)
	hasLast bool
	stopped bool

	// overflow classification (v0.9.9): classify/stage may be nil
	// (everything Replaceable, no pending merging).
	classify func(T) Class
	stage    func(T) string

	// signal wakes the delivery goroutine (buffered, capacity 1: a
	// second Publish while one signal is pending needs no second
	// signal — the goroutine drains the whole queue anyway).
	signal chan struct{}
	// quit terminates the delivery goroutine after a final drain.
	quit chan struct{}
	done chan struct{}
}

// New creates a publisher. equal is the semantic-equality predicate
// used for duplicate suppression (nil = every snapshot is treated as
// changed). The delivery goroutine starts immediately; pair New with
// Stop (the composition root stops publishers before the UI runtime
// is destroyed).
func New[T any](name string, equal func(a, b T) bool, opts ...Option[T]) *Publisher[T] {
	applied := publisherOptions[T]{}
	for _, opt := range opts {
		if opt != nil {
			opt(&applied)
		}
	}

	p := &Publisher[T]{
		name:     name,
		equal:    equal,
		classify: applied.classify,
		stage:    applied.stage,
		signal:   make(chan struct{}, 1),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	go p.loop()

	return p
}

// Subscribe registers a listener that receives every delivered
// snapshot. The returned cancel function unregisters it. Listeners
// must be cheap and non-blocking: they run on the delivery goroutine.
// A nil listener registers nothing.
func (p *Publisher[T]) Subscribe(fn func(T)) (cancel func()) {
	if fn == nil {
		return func() {}
	}

	p.subMu.Lock()

	if p.subscribers == nil {
		p.subscribers = make(map[int]func(T))
	}

	p.subSeq++
	id := p.subSeq
	p.subscribers[id] = fn

	p.subMu.Unlock()

	return func() {
		p.subMu.Lock()
		delete(p.subscribers, id)
		p.subMu.Unlock()
	}
}

// Publish submits a snapshot. Semantics:
//
//   - stopped → no-op;
//   - semantically equal to the newest submitted snapshot → dropped
//     (identical state, whether still pending or already delivered,
//     never re-emits);
//   - otherwise the snapshot is appended to the pending queue and the
//     delivery goroutine is woken (non-blocking).
//
// Publish never blocks on the consumer and never blocks on the
// caller's other locks: it takes only p.mu, which the delivery path
// holds only for O(1) queue operations.
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

	item := queueItem[T]{snapshot: snapshot}
	if p.classify != nil {
		item.class = p.classify(snapshot)

		if p.stage != nil {
			item.stage = p.stage(snapshot)
		}
	}

	if len(p.queue) >= maxQueue {
		// Overflow valve (v0.9.9): compact instead of blind
		// drop-oldest — merge same-stage entries, then shed
		// replaceable ones — so lifecycle transitions survive a
		// stalled consumer (see the package contract).
		p.compactLocked()

		if len(p.queue) >= maxQueue {
			// Absolute last resort: the queue still holds only
			// distinct critical stages beyond the bound. Drop the
			// OLDEST entry to preserve the bound and the newest
			// snapshot.
			copy(p.queue, p.queue[1:])
			p.queue[len(p.queue)-1] = item
		} else {
			p.queue = append(p.queue, item)
		}
	} else {
		p.queue = append(p.queue, item)
	}

	p.last = snapshot
	p.hasLast = true

	select {
	case p.signal <- struct{}{}:
	default:
	}

	p.mu.Unlock()
}

// Stop terminates the publisher synchronously and idempotently.
// Snapshots published before Stop are drained and delivered before
// the delivery goroutine exits (the terminal-state guarantee). After
// the first Stop returns: every later Publish is a no-op and no
// callback can run again. This is the guarantee that lets the
// composition root stop publishers BEFORE the UI runtime is
// destroyed — no callback ever fires into a dead event target.
func (p *Publisher[T]) Stop() {
	p.mu.Lock()

	if p.stopped {
		p.mu.Unlock()

		<-p.done // already stopping/stopped: just join

		return
	}

	p.stopped = true
	close(p.quit)

	p.mu.Unlock()

	<-p.done
}

// Name returns the publisher's diagnostic name.
func (p *Publisher[T]) Name() string {
	return p.name
}

// loop is the single delivery goroutine. It drains the pending queue
// in order on every wake and applies a final drain before exiting on
// quit, so no published snapshot is left behind by Stop.
func (p *Publisher[T]) loop() {
	defer close(p.done)

	for {
		select {
		case <-p.signal:
			p.drain()
		case <-p.quit:
			// Final drain: deliver everything published before Stop.
			p.drain()

			return
		}
	}
}

// drain delivers every queued snapshot in publication order. Emission
// happens outside p.mu; a slow consumer delays later deliveries but
// can never block a publisher (the queue is bounded).
func (p *Publisher[T]) drain() {
	for {
		p.mu.Lock()

		if len(p.queue) == 0 {
			p.mu.Unlock()

			return
		}

		item := p.queue[0]
		p.queue = p.queue[1:]

		p.mu.Unlock()

		p.dispatch(item.snapshot)
	}
}

// compactLocked makes room in the pending queue WITHOUT silently
// dropping lifecycle transitions (caller holds p.mu):
//
//  1. merge consecutive pending snapshots of the SAME stage (newest
//     wins) — repeated re-publication of one stage collapses;
//  2. shed REPLACEABLE entries, oldest first, until one slot is free.
//
// Critical stages keep their relative order and (barring the absolute
// last resort in Publish) are all preserved.
func (p *Publisher[T]) compactLocked() {
	// 1. merge consecutive same-stage entries.
	if p.stage != nil && len(p.queue) > 1 {
		merged := make([]queueItem[T], 0, len(p.queue))

		for _, item := range p.queue {
			if n := len(merged); n > 0 &&
				merged[n-1].stage != "" && merged[n-1].stage == item.stage {
				merged[n-1] = item // newest of the stage wins

				continue
			}

			merged = append(merged, item)
		}

		p.queue = merged
	}

	if len(p.queue) < maxQueue {
		return
	}

	// 2. shed replaceable entries, oldest first.
	shed := len(p.queue) - maxQueue + 1 // free at least one slot

	kept := make([]queueItem[T], 0, len(p.queue))

	for _, item := range p.queue {
		if shed > 0 && item.class == Replaceable {
			shed--

			continue
		}

		kept = append(kept, item)
	}

	p.queue = kept
}

// dispatch runs every registered listener with the snapshot, outside
// all locks.
func (p *Publisher[T]) dispatch(snapshot T) {
	p.subMu.Lock()

	listeners := make([]func(T), 0, len(p.subscribers))
	for _, fn := range p.subscribers {
		listeners = append(listeners, fn)
	}

	p.subMu.Unlock()

	for _, fn := range listeners {
		fn(snapshot)
	}
}
