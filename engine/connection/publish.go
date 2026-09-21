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
//   - lifecycle-critical transitions are never silently dropped by
//     the queue valve (v0.9.9: critical/replaceable classification;
//     only same-stage pending snapshots merge, replaceable telemetry
//     coalesces under a stalled consumer);
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
	"github.com/Parsaetak/FreeIran/internal/statepub"
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

// snapshotClass classifies connection snapshots for the publisher's
// overflow policy (v0.9.9):
//
//   - CRITICAL — real lifecycle transitions, never silently dropped:
//     selecting, preparing, starting_core, waiting_for_ready,
//     verifying, connected_verified, disconnecting, disconnected,
//     connection_failed;
//   - REPLACEABLE — the connected (pre-verification) state and pure
//     telemetry mutations (latency/verification-evidence updates on an
//     unchanged state) coalesce under a stalled consumer; the newest
//     full snapshot always carries the authoritative values.
func snapshotClass(s Snapshot) statepub.Class {
	switch s.State {
	case StateSelecting, StatePreparing, StateStartingCore,
		StateWaitingForReady, StateVerifying, StateConnectedVerified,
		StateDisconnecting, StateDisconnected, StateConnectionFailed:
		return statepub.Critical
	default:
		return statepub.Replaceable
	}
}

// snapshotStage is the coalescing stage of a connection snapshot: two
// pending snapshots of the same lifecycle state merge (newest wins).
func snapshotStage(s Snapshot) string {
	return string(s.State)
}

// snapshotsEqual is the semantic-equality predicate for connection
// snapshots: identical content = identical event. v0.9.9 replaces the
// hot-path reflect.DeepEqual with an explicit field comparison over
// exactly the observable value families — state, identity, evidence,
// telemetry and attempt history — so irrelevant/internal changes can
// never suppress a real event and reflection costs nothing.
func snapshotsEqual(a, b Snapshot) bool {
	if a.State != b.State ||
		a.Core != b.Core ||
		a.CoreVersion != b.CoreVersion ||
		a.ConfigID != b.ConfigID ||
		a.ConfigName != b.ConfigName ||
		a.ConfigDisplay != b.ConfigDisplay ||
		a.Endpoint != b.Endpoint ||
		a.LatencyMS != b.LatencyMS ||
		a.StartedAt != b.StartedAt ||
		a.LastError != b.LastError ||
		a.FallbacksUsed != b.FallbacksUsed ||
		a.CorePID != b.CorePID ||
		a.CoreReadyMS != b.CoreReadyMS ||
		a.PingMedianMS != b.PingMedianMS ||
		a.URLTotalMS != b.URLTotalMS ||
		a.Verification != b.Verification ||
		a.VerifiedAt != b.VerifiedAt ||
		a.VerifyFailures != b.VerifyFailures {
		return false
	}

	if len(a.Attempts) != len(b.Attempts) {
		return false
	}

	for i := range a.Attempts {
		if a.Attempts[i] != b.Attempts[i] {
			return false
		}
	}

	return true
}
