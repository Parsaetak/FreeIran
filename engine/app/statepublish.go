// statepublish.go implements the application-level state publishing
// wiring: the event-driven UI synchronization between the engine's
// authoritative transitions and the desktop runtime.
//
// Two authoritative streams feed the UI:
//
//	freeiran:state       ← AppState            (application state)
//	freeiran:connection  ← connection.Snapshot (state machine)
//
// The connection manager OWNS its snapshot publisher
// (engine/connection/publish.go) and pushes every real transition
// into it. SetConnectionListener only subscribes the UI bridge to
// that stream. The application-state publisher lives here. Both are
// internal/statepub publishers: semantic duplicate suppression (same
// state = no event), ordered zero-delay delivery (lifecycle
// transitions are never silently coalesced away) and a synchronous,
// draining Stop. The engine never imports the UI runtime; only the
// composition root (cmd/freeiran) registers the emit callbacks.
//
// Lifecycle: the state publisher and the connection subscription are
// stopped synchronously inside App.Shutdown — the connection manager
// has already delivered its terminal snapshot and joined its own
// publisher by then — so after Shutdown returns no emit callback can
// run and no publisher goroutine survives.
package app

import (
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/pipeline"
	"github.com/Parsaetak/FreeIran/engine/testqueue"
	"github.com/Parsaetak/FreeIran/internal/statepub"
)

// SetStateListener registers the emit callback for application-state
// changes (AppState snapshots). The first registration immediately
// publishes the CURRENT state — the UI converges at registration time
// — and every subsequent real change is published from the transition
// path that caused it. Registering twice is rejected (the composition
// root wires exactly one listener; a second registration would leak a
// second publisher goroutine).
func (a *App) SetStateListener(fn func(AppState)) {
	if fn == nil {
		return
	}

	a.pubMu.Lock()

	if a.statePub != nil {
		a.pubMu.Unlock()

		if a.logger != nil {
			a.logger.Warn("app", "state_publisher",
				"SetStateListener called twice; ignoring the second registration")
		}

		return
	}

	a.statePub = statepub.New("app-state", appStateEqual)
	a.statePub.Subscribe(fn)

	a.pubMu.Unlock()

	// Initial authoritative snapshot: the UI converges at
	// registration time without waiting for any transition.
	a.publishState()
}

// SetConnectionListener registers the emit callback for connection
// state-machine snapshots. The manager's publisher dispatches after
// every real transition — deduplicated, ordered, zero-delay — and the
// current snapshot is (re-)published through the same stream at
// registration, so the UI converges immediately. Registering twice is
// rejected (a second subscription would outlive the first one's
// teardown and leak delivery work).
func (a *App) SetConnectionListener(fn func(connection.Snapshot)) {
	if fn == nil {
		return
	}

	a.pubMu.Lock()

	if a.connSubCancel != nil {
		a.pubMu.Unlock()

		if a.logger != nil {
			a.logger.Warn("app", "connection_listener",
				"SetConnectionListener called twice; ignoring the second registration")
		}

		return
	}

	// Subscribe + initial convergence in one step: the manager
	// re-publishes the current snapshot through its ordered stream
	// (deduplicated when nothing changed since the last delivery).
	cancel := a.connMgr.Subscribe(fn)

	a.connSubCancel = cancel

	a.pubMu.Unlock()
}

// publishState pushes the current application state into the
// deduplicating publisher (no-op until a listener registered). Called
// from every meaningful state-transition path: boot phases,
// degraded/healthy transitions, ingestion start/finish, shutdown.
func (a *App) publishState() {
	a.pubMu.Lock()
	pub := a.statePub
	a.pubMu.Unlock()

	if pub == nil {
		return
	}

	pub.Publish(a.State())
}

// stopPublishers terminates the UI wiring synchronously. Called in
// App.Shutdown after the connection manager joined its own publisher
// (final disconnected snapshot delivered): the connection subscription
// is cancelled and the application-state publisher is stopped after
// draining its pending snapshots — after this returns no emit
// callback can run again.
func (a *App) stopPublishers() {
	a.pubMu.Lock()
	statePub := a.statePub
	cancel := a.connSubCancel
	queueCancel := a.queueWatchCancel

	a.statePub = nil
	a.connSubCancel = nil
	a.queueWatchCancel = nil
	a.queueStateListener = nil
	a.pubMu.Unlock()

	if queueCancel != nil {
		queueCancel()
	}

	if cancel != nil {
		cancel()
	}

	if statePub != nil {
		statePub.Stop()
	}
}

// SetQueueStateListener registers the emit callback for the ONE
// authoritative queue-state stream (v0.9.15): complete
// testqueue.LiveStateView projections pushed on every queue change
// (coalesced — the newest complete state wins) plus one immediate
// convergence publish at registration. The composition root
// (cmd/freeiran) bridges it to the freeiran:queuestate UI event; the
// UI's recovery read is the LiveState binding — event and recovery
// read share the SAME shape and semantics. Registering twice is
// rejected, mirroring SetStateListener.
func (a *App) SetQueueStateListener(fn func(testqueue.LiveStateView)) {
	if fn == nil {
		return
	}

	a.pubMu.Lock()

	if a.queueStateListener != nil {
		a.pubMu.Unlock()

		if a.logger != nil {
			a.logger.Warn("app", "queue_state_listener",
				"SetQueueStateListener called twice; ignoring the second registration")
		}

		return
	}

	a.queueStateListener = fn
	a.pubMu.Unlock()

	// Convergence at registration: publish the CURRENT complete state
	// when a queue already exists (the lazy construction has not run
	// in a fresh session — then the first queue creation publishes).
	if q := a.currentQueue(); q != nil {
		fn(liveStateView(q))
	}
}

// currentQueue returns the app's test queue, if constructed.
func (a *App) currentQueue() *testqueue.Queue {
	a.initMu.Lock()
	defer a.initMu.Unlock()

	return a.testQueue
}

// queueHas reports whether the shared test queue currently has a task
// for the fingerprint (pending or in flight). Satellite test paths
// (discovery candidate warm-up, Quick Connect shortlist retest) use it
// as a one-engine guard: a config the queue is already testing is
// never concurrently retested, so the queue's persisted result stays
// the single last writer. Nil-queue-safe.
func (a *App) queueHas(fingerprint string) bool {
	if q := a.currentQueue(); q != nil {
		return q.Queued(fingerprint)
	}

	return false
}

// startQueueStateWatcher runs the ONE queue-change pump: it follows
// the current queue across SetMode swaps, waits for coalesced change
// signals and publishes the newest complete LiveStateView to the
// registered listener. Started exactly once from ensureTestQueue;
// exits when the app context is cancelled or stopPublishers clears
// the listener. A burst of changes coalesces into one publish of the
// newest state (the queue's change channel has capacity 1), so a
// 16,000-config batch publishes at observation rate, not mutation
// rate.
func (a *App) startQueueStateWatcher(initial *testqueue.Queue) {
	go func() {
		q := initial

		for {
			if q == nil {
				return
			}

			changes, cancel := q.SubscribeChanges()

			a.pubMu.Lock()
			a.queueWatchCancel = cancel
			listener := a.queueStateListener
			a.pubMu.Unlock()

			// Initial convergence for this queue instance.
			if listener != nil {
				listener(liveStateView(q))
			}

			select {
			case <-a.ctx.Done():
				cancel()
				return
			case <-changes:
			}

			// A change arrived: publish the newest complete state. The
			// listener may have been cleared by shutdown — publishing
			// is then skipped, but the loop keeps following the queue
			// until the app context ends (the queue itself is owned by
			// the same lifecycle).
			a.pubMu.Lock()
			listener = a.queueStateListener
			a.pubMu.Unlock()

			if listener != nil {
				listener(liveStateView(q))
			}

			// Follow a SetMode swap: the old queue's Stop issued one
			// final change signal above (publishing its drained state);
			// the next iteration binds to whatever queue is current now.
			cancel()

			if next := a.currentQueue(); next != nil && next != q {
				q = next
			}
		}
	}()
}

// liveStateView builds the complete UI projection from one queue:
// the authoritative live set + version + the aggregate stats and
// pause flag — the ONE shape shared by the event stream and the
// LiveState recovery binding.
func liveStateView(q *testqueue.Queue) testqueue.LiveStateView {
	live := q.LiveState()

	return testqueue.LiveStateView{
		Version:      live.Version,
		Fingerprints: live.Fingerprints,
		Stats:        q.Stats(),
		Paused:       q.Paused(),
	}
}

// appStateEqual is the semantic-equality predicate for AppState
// snapshots: identical content = identical event. v0.9.9 replaces the
// hot-path reflect.DeepEqual with an explicit comparison of the
// observable value families (status, identity, boot phase, counts,
// storage and ingestion stats) — the publisher drops a snapshot only
// when NOTHING observable changed.
func appStateEqual(a, b AppState) bool {
	if a.Status != b.Status ||
		a.Version != b.Version ||
		a.Identity != b.Identity ||
		a.StartedAt != b.StartedAt ||
		a.ConfigCount != b.ConfigCount ||
		a.IngestionRunning != b.IngestionRunning ||
		a.NativeAcceler != b.NativeAcceler ||
		a.BootPhase != b.BootPhase ||
		len(a.BootTimings) != len(b.BootTimings) {
		return false
	}

	for k, v := range a.BootTimings {
		if bv, ok := b.BootTimings[k]; !ok || bv != v {
			return false
		}
	}

	if a.Storage != b.Storage {
		return false
	}

	if (a.LastIngestion == nil) != (b.LastIngestion == nil) {
		return false
	}

	if a.LastIngestion != nil && !pipelineStatsEqual(*a.LastIngestion, *b.LastIngestion) {
		return false
	}

	return true
}

// pipelineStatsEqual compares ingestion stats semantically
// (PerSource is compared element-wise; a nil slice and an empty slice
// are the same observation).
func pipelineStatsEqual(a, b pipeline.Stats) bool {
	if a.SourcesTotal != b.SourcesTotal ||
		a.SourcesOK != b.SourcesOK ||
		a.SourcesFailed != b.SourcesFailed ||
		a.SourcesUnchanged != b.SourcesUnchanged ||
		a.Discovered != b.Discovered ||
		a.Duplicates != b.Duplicates ||
		a.Persisted != b.Persisted ||
		a.Invalid != b.Invalid ||
		len(a.PerSource) != len(b.PerSource) {
		return false
	}

	for i := range a.PerSource {
		if a.PerSource[i] != b.PerSource[i] {
			return false
		}
	}

	return true
}
