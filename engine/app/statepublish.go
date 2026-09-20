// statepublish.go implements the application-level state publishers
// (v0.9.8.7): the event-driven UI synchronization that replaced the
// 2-second ticker broadcasts in the desktop entrypoint.
//
// Two authoritative streams feed the UI:
//
//	freeiran:state       ← Publisher[AppState]        (application state)
//	freeiran:connection  ← Publisher[connection.Snapshot] (state machine)
//
// Both go through internal/statepub: semantic duplicate suppression
// (same state = no event) plus a short coalescing window (a burst of
// transitions collapses into one emission of the newest snapshot).
// The composition root registers the emit callbacks (SetStateListener
// / SetConnectionListener) — the engine itself never imports the UI
// runtime, exactly like the existing core-progress and start-flow
// listeners.
//
// Lifecycle: the publishers are stopped synchronously inside
// App.Shutdown BEFORE the runtime log closes — after Shutdown returns
// no emit callback can run, no publisher goroutine survives, and no
// callback fires into a destroyed UI runtime.
package app

import (
	"reflect"

	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/internal/statepub"
)

// SetStateListener registers the emit callback for application-state
// changes (AppState snapshots). The first registration immediately
// emits the CURRENT state — the "initial broadcast" is event-driven,
// not a timed sleep — and every subsequent real change is published
// from the transition path that caused it. Registering twice is
// rejected (the composition root wires exactly one listener; a second
// registration would leak the first publisher's goroutine).
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

	a.statePub = statepub.New("app-state", fn, appStateEqual, statepub.DefaultCoalesce)

	a.pubMu.Unlock()

	// Initial authoritative snapshot: the UI converges at
	// registration time without waiting for any ticker.
	a.publishState()
}

// SetConnectionListener registers the emit callback for connection
// state-machine changes. The manager's subscription dispatches after
// every real transition; this publisher deduplicates and coalesces
// before the UI is touched. The first registration emits the current
// snapshot immediately.
func (a *App) SetConnectionListener(fn func(connection.Snapshot)) {
	if fn == nil {
		return
	}

	a.pubMu.Lock()

	if a.connPub != nil {
		a.pubMu.Unlock()

		if a.logger != nil {
			a.logger.Warn("app", "connection_publisher",
				"SetConnectionListener called twice; ignoring the second registration")
		}

		return
	}

	a.connPub = statepub.New("connection", fn, connectionSnapshotEqual, statepub.DefaultCoalesce)

	a.pubMu.Unlock()

	// Bridge: manager transition → deduplicating publisher.
	cancel := a.connMgr.Subscribe(func(snapshot connection.Snapshot) {
		a.pubMu.Lock()
		pub := a.connPub
		a.pubMu.Unlock()

		if pub != nil {
			pub.Publish(snapshot)
		}
	})

	a.pubMu.Lock()
	a.connSubCancel = cancel
	a.pubMu.Unlock()

	// Initial authoritative snapshot.
	a.connPub.Publish(a.connMgr.Snapshot())
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

// stopPublishers terminates both publishers synchronously. Called in
// App.Shutdown after the connection manager joined its dispatcher:
// after this returns no emit callback can run again.
func (a *App) stopPublishers() {
	a.pubMu.Lock()
	statePub := a.statePub
	connPub := a.connPub
	cancel := a.connSubCancel

	a.statePub = nil
	a.connPub = nil
	a.connSubCancel = nil
	a.pubMu.Unlock()

	if cancel != nil {
		cancel()
	}

	if connPub != nil {
		connPub.Stop()
	}

	if statePub != nil {
		statePub.Stop()
	}
}

// appStateEqual is the semantic-equality predicate for AppState
// snapshots: identical content = identical event. reflect.DeepEqual
// compares the value families that matter (status, boot phase,
// storage stats, ingestion stats) — the publisher drops a snapshot
// only when NOTHING observable changed.
func appStateEqual(a, b AppState) bool {
	return reflect.DeepEqual(a, b)
}

// connectionSnapshotEqual is the semantic-equality predicate for
// connection snapshots. DeepEqual covers the state string, core
// identity, latency/verification evidence and the attempt history.
func connectionSnapshotEqual(a, b connection.Snapshot) bool {
	return reflect.DeepEqual(a, b)
}
