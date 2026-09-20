// publish.go connects the connection manager to its snapshot
// publisher (internal/statepub) — the single dispatch boundary
// between authoritative state mutations and the UI:
//
//	AUTHORITATIVE STATE MUTATION (under m.mu)
//	        ↓  stateChanged() — snapshot pushed under the lock
//	STATEPUB PUBLISHER — ordered, deduplicated, zero-delay delivery
//	        ↓
//	SUBSCRIBERS (the composition root bridges to the UI runtime)
//
// Guarantees (statepub):
//
//   - every real transition is delivered, in order, with no
//     artificial delay — user-visible lifecycle states
//     (preparing / starting_core / waiting_for_ready) are never
//     silently lost inside a coalescing window;
//   - identical snapshots are suppressed (same state = no event);
//   - stopPublisher is synchronous and drains pending snapshots
//     first: after Shutdown returns, the final terminal snapshot has
//     been delivered and no listener callback can run again against
//     a torn-down application.
package connection

import (
	"reflect"
)

// Subscribe registers a listener that receives the authoritative
// connection Snapshot after every real state transition. The returned
// cancel function unregisters it. The current snapshot is re-published
// through the same ordered stream at registration, so a new subscriber
// converges immediately (deduplicated when nothing changed). Listeners
// must be cheap and non-blocking: they run on the publisher's delivery
// goroutine. A nil listener registers nothing.
func (m *Manager) Subscribe(fn func(Snapshot)) (cancel func()) {
	if m == nil || m.publisher == nil || fn == nil {
		return func() {}
	}

	cancel = m.publisher.Subscribe(fn)

	// Initial convergence: push the current authoritative snapshot
	// through the same ordered, deduplicated stream (a no-op when it
	// equals the newest already-delivered snapshot).
	m.publisher.Publish(m.Snapshot())

	return cancel
}

// stateChanged publishes the authoritative snapshot. Called with m.mu
// HELD (every mutation site), so the pushed snapshot is a consistent
// view of the transition that just happened; delivery itself happens
// on the publisher's goroutine, outside all locks. Snapshots are
// deduplicated semantically — a mutation that changes nothing
// observable emits nothing.
func (m *Manager) stateChanged() {
	if m == nil || m.publisher == nil {
		return
	}

	m.publisher.Publish(m.snapshotLocked())
}

// stopPublisher terminates the publisher synchronously and
// idempotently. Called by Shutdown after the terminal state
// transitions: pending snapshots (the final disconnected state in
// particular) are drained and delivered BEFORE the publisher
// terminates. After it returns, no listener callback can run again.
func (m *Manager) stopPublisher() {
	if m == nil || m.publisher == nil {
		return
	}

	m.publisher.Stop()
}

// snapshotsEqual is the semantic-equality predicate for connection
// snapshots: identical content = identical event. DeepEqual covers
// the state string, core identity, latency/verification evidence and
// the attempt history — the publisher drops a snapshot only when
// NOTHING observable changed.
func snapshotsEqual(a, b Snapshot) bool {
	return reflect.DeepEqual(a, b)
}
