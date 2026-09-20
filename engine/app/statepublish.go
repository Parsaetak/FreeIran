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
	"reflect"

	"github.com/Parsaetak/FreeIran/engine/connection"
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

	a.statePub = nil
	a.connSubCancel = nil
	a.pubMu.Unlock()

	if cancel != nil {
		cancel()
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
