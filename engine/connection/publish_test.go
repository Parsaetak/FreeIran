// publish_test.go pins the v0.9.8.7 snapshot-subscription contract:
// every real state-machine transition wakes the dispatch loop, which
// delivers the NEWEST authoritative snapshot — event-driven, no
// periodic heartbeat. Bursts of sub-millisecond transitions collapse
// into newer snapshots by design (the app-layer publisher dedups and
// coalesces for the UI); the delivered sequence is therefore
// monotonic in lifecycle order and always terminates with the
// authoritative terminal state.
package connection_test

import (
	"context"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
)

// lifecycleRank orders the connection states for the monotonicity
// assertion (a later dispatch must never carry an EARLIER lifecycle
// position within one session).
func lifecycleRank(s connection.State) int {
	switch s {
	case connection.StateDisconnected:
		return 0
	case connection.StateSelecting:
		return 1
	case connection.StatePreparing:
		return 2
	case connection.StateStartingCore:
		return 3
	case connection.StateWaitingForReady:
		return 4
	case connection.StateConnected:
		return 5
	case connection.StateVerifying:
		return 6
	case connection.StateConnectedVerified:
		return 7
	case connection.StateDisconnecting:
		return 8
	case connection.StateConnectionFailed:
		return 9
	}

	return -1
}

// TestSubscribeReceivesTransitionsWithoutHeartbeat pins the
// event-propagation contract from §15: a successful connect's
// terminal state reaches the subscriber WITHOUT any ticker, through
// monotonic snapshots of the real state machine (selecting → … →
// connected; sub-millisecond intermediates may coalesce into newer
// snapshots, but none may regress).
func TestSubscribeReceivesTransitionsWithoutHeartbeat(t *testing.T) {
	manager, _ := testEnv(t)
	defer manager.Shutdown()

	events := make(chan connection.Snapshot, 64)
	cancel := manager.Subscribe(func(s connection.Snapshot) { events <- s })
	defer cancel()

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v (state %s)", err, snapshot.State)
	}

	deadline := time.After(5 * time.Second)

	lastRank := -1
	connected := false

	for !connected {
		select {
		case s := <-events:
			rank := lifecycleRank(s.State)

			if rank < 0 {
				t.Fatalf("subscriber observed unknown state %q", s.State)
			}

			// Monotonic within the session: a coalescing dispatcher
			// may skip intermediate states but never goes backwards.
			if rank < lastRank && s.State != connection.StateConnectionFailed {
				t.Fatalf("state regression: %q after rank %d", s.State, lastRank)
			}

			lastRank = rank

			if s.State == connection.StateConnected {
				connected = true
			}
		case <-deadline:
			t.Fatal("connected snapshot not delivered within 5s — events are not reaching the subscriber without a heartbeat")
		}
	}
}

// TestSubscribeDisconnectDelivered pins disconnect propagation:
// disconnecting while connected delivers the authoritative
// disconnected snapshot to subscribers.
func TestSubscribeDisconnectDelivered(t *testing.T) {
	manager, _ := testEnv(t)
	defer manager.Shutdown()

	events := make(chan connection.Snapshot, 64)
	cancel := manager.Subscribe(func(s connection.Snapshot) { events <- s })
	defer cancel()

	if _, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{}); err != nil {
		t.Fatalf("Connect() failed: %v", err)
	}

	manager.Disconnect()

	deadline := time.After(3 * time.Second)

	for {
		select {
		case s := <-events:
			if s.State == connection.StateDisconnected {
				return
			}
		case <-deadline:
			t.Fatal("disconnected snapshot not delivered after Disconnect")
		}
	}
}

// TestSubscribeUnsubscribeStopsDelivery pins the cancel contract.
func TestSubscribeUnsubscribeStopsDelivery(t *testing.T) {
	manager, _ := testEnv(t)
	defer manager.Shutdown()

	events := make(chan connection.Snapshot, 64)
	cancel := manager.Subscribe(func(s connection.Snapshot) { events <- s })

	if _, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{}); err != nil {
		t.Fatalf("Connect() failed: %v", err)
	}

	// Wait for the connect burst to drain.
	drain := time.After(1 * time.Second)

drainLoop:
	for {
		select {
		case <-events:
		case <-drain:
			break drainLoop
		}
	}

	cancel()

	// A fresh burst of transitions: none may reach the cancelled
	// subscriber.
	manager.Disconnect()

	select {
	case s := <-events:
		t.Fatalf("cancelled subscriber received state %q", s.State)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestStopPublisherJoinsOnShutdown pins the shutdown guarantee: after
// Manager.Shutdown returns, no listener callback can still be running
// and no dispatch goroutine survives.
func TestStopPublisherJoinsOnShutdown(t *testing.T) {
	manager, _ := testEnv(t)

	blocked := make(chan struct{})
	release := make(chan struct{})

	manager.Subscribe(func(connection.Snapshot) {
		// Signal once (the first callback only) and hold the
		// dispatcher inside the callback.
		select {
		case <-blocked:
		default:
			close(blocked)
		}

		<-release
	})

	shutdownDone := make(chan struct{})

	go func() {
		manager.Shutdown()
		close(shutdownDone)
	}()

	<-blocked // a callback is running inside the dispatch loop

	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while a listener callback was still running")
	case <-time.After(200 * time.Millisecond):
		// expected: Shutdown is joined on the dispatcher
	}

	close(release)

	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the callback finished")
	}
}

// TestShutdownDuringVerificationStillDeliversTerminalState pins the
// §15 shutdown propagation: a Shutdown while a session runs still
// delivers the final disconnected snapshot before the dispatcher
// stops (the composition root may forward it to the UI).
func TestShutdownDuringVerificationStillDeliversTerminalState(t *testing.T) {
	manager, _ := testEnv(t)

	events := make(chan connection.Snapshot, 64)
	cancel := manager.Subscribe(func(s connection.Snapshot) { events <- s })
	defer cancel()

	if _, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{}); err != nil {
		t.Fatalf("Connect() failed: %v", err)
	}

	manager.Shutdown()

	deadline := time.After(3 * time.Second)

	for {
		select {
		case s := <-events:
			if s.State == connection.StateDisconnected {
				return
			}
		case <-deadline:
			t.Fatal("terminal disconnected snapshot not delivered across Shutdown")
		}
	}
}
