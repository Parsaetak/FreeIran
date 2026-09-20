// publish.go implements the connection manager's snapshot
// subscription mechanism (v0.9.8.7): the event-driven UI
// synchronization path that replaced the 2-second ticker broadcasts.
//
// Model:
//
//	AUTHORITATIVE STATE MUTATION (under m.mu)
//	        ↓  stateChanged() — non-blocking wake signal
//	DISPATCH GOROUTINE (builds the CURRENT Snapshot — the newest
//	state, never a stale intermediate)
//	        ↓
//	LISTENERS (the app layer feeds its deduplicating publisher,
//	which coalesces bursts and forwards to the UI runtime)
//
// Guarantees:
//
//   - Every real mutation wakes the dispatcher; a burst of mutations
//     collapses into a bounded number of dispatches of the NEWEST
//     authoritative snapshot (older pending signals are superseded,
//     never queued).
//   - Listeners run OUTSIDE both locks: a slow consumer can never
//     block a state transition or deadlock the manager.
//   - stopPublisher is synchronous: after Shutdown returns, the
//     dispatch goroutine has exited and no listener callback runs
//     against a torn-down application.
package connection

// Subscribe registers a listener that receives the authoritative
// connection Snapshot after every real state mutation. The returned
// cancel function unregisters it. Listeners must be cheap and
// non-blocking: they run on the manager's dispatch goroutine (the
// app-layer publisher applies its own dedup/coalescing before the UI
// is touched). A nil listener registers nothing.
func (m *Manager) Subscribe(fn func(Snapshot)) (cancel func()) {
	if fn == nil {
		return func() {}
	}

	m.subMu.Lock()
	defer m.subMu.Unlock()

	if m.subscribers == nil {
		m.subscribers = make(map[int]func(Snapshot))
	}

	m.subSeq++
	id := m.subSeq
	m.subscribers[id] = fn

	return func() {
		m.subMu.Lock()
		delete(m.subscribers, id)
		m.subMu.Unlock()
	}
}

// stateChanged wakes the dispatch goroutine. Called with m.mu HELD
// (every mutation site), so the stopped check serializes against
// stopPublisher's channel close — a send can never race a close.
// Non-blocking: a signal already pending collapses the mutations that
// follow it into one dispatch of the newest snapshot.
func (m *Manager) stateChanged() {
	if m.pubStopped || m.notifyCh == nil {
		return
	}

	select {
	case m.notifyCh <- struct{}{}:
	default:
	}
}

// publishLoop is the single dispatch goroutine started by New. Each
// wake builds the CURRENT authoritative snapshot (not the intermediate
// state that triggered the wake — a newer one already replaced it) and
// delivers it to every subscriber outside all locks.
func (m *Manager) publishLoop() {
	defer close(m.pubDone)

	for range m.notifyCh {
		snapshot := m.Snapshot()

		m.subMu.Lock()
		listeners := make([]func(Snapshot), 0, len(m.subscribers))
		for _, fn := range m.subscribers {
			listeners = append(listeners, fn)
		}
		m.subMu.Unlock()

		for _, fn := range listeners {
			fn(snapshot)
		}
	}
}

// stopPublisher terminates the dispatch goroutine synchronously and
// idempotently. Called by Shutdown after the terminal state
// transitions so the final disconnected snapshot is still delivered.
// After it returns: no listener callback can run again.
func (m *Manager) stopPublisher() {
	m.mu.Lock()

	if m.pubStopped || m.notifyCh == nil {
		m.mu.Unlock()

		return
	}

	m.pubStopped = true
	close(m.notifyCh)

	m.mu.Unlock()

	<-m.pubDone
}
