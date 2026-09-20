// statepublish_test.go pins the v0.9.8.7 application-level publisher
// contract:
//
//	SetStateListener delivers the current state at registration
//	boot-phase advances reach the listener as real events
//	duplicate snapshots are suppressed (no event storm)
//	no callback runs after Shutdown (publisher joined)
package app

import (
	"sync"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
)

// adHocTestConfig returns a minimal VLESS config used to drive a real
// (failing — no cores are installed) connect through the state
// machine. The registry has no backends, so Select fails and the
// machine reaches connection_failed deterministically.
func adHocTestConfig() config.Config {
	c := config.Config{
		Name: "publisher-test",
		Type: config.TypeVLESS,
	}
	c.SetID()

	return c
}

// stateCollector gathers AppState emissions behind a mutex.
type stateCollector struct {
	mu     sync.Mutex
	states []AppState
	closed chan struct{}
	once   sync.Once
}

func newStateCollector() *stateCollector {
	return &stateCollector{closed: make(chan struct{})}
}

func (c *stateCollector) emit(s AppState) {
	c.mu.Lock()
	c.states = append(c.states, s)
	c.mu.Unlock()

	c.once.Do(func() { close(c.closed) })
}

func (c *stateCollector) snapshot() []AppState {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]AppState(nil), c.states...)
}

func (c *stateCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.states)
}

func waitForCount(t *testing.T, c *stateCollector, want int, deadline time.Duration) {
	t.Helper()

	deadlineAt := time.Now().Add(deadline)

	for time.Now().Before(deadlineAt) {
		if c.count() >= want {
			return
		}

		time.Sleep(2 * time.Millisecond)
	}

	t.Fatalf("listener received %d events within %v, want >= %d", c.count(), deadline, want)
}

// TestStateListenerReceivesInitialStateAndBootPhases pins: the first
// registration emits the CURRENT state immediately (no timed sleep),
// and a subsequent boot-phase advance (MarkUIReady) is published as a
// real event that reaches the listener without any heartbeat.
func TestStateListenerReceivesInitialStateAndBootPhases(t *testing.T) {
	a := newTestApp(t)
	defer a.Shutdown()

	c := newStateCollector()
	a.SetStateListener(c.emit)

	// Registration itself published the current snapshot.
	waitForCount(t, c, 1, 2*time.Second)

	before := c.snapshot()[0]

	if before.Status != "ready" {
		t.Fatalf("initial published status = %q, want ready", before.Status)
	}

	// A boot-phase advance is a real state transition.
	a.MarkUIReady()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		for _, s := range c.snapshot() {
			if s.BootPhase == BootUIReady {
				return
			}
		}

		time.Sleep(2 * time.Millisecond)
	}

	t.Fatalf("boot-phase advance to %q not delivered; events: %d", BootUIReady, c.count())
}

// TestStateListenerDuplicateSuppression pins the dedup contract: a
// no-op MarkUIReady / MarkUIRuntimeReady re-run (idempotent calls that
// do NOT advance the phase) must NOT produce additional events.
func TestStateListenerDuplicateSuppression(t *testing.T) {
	a := newTestApp(t)
	defer a.Shutdown()

	c := newStateCollector()
	a.SetStateListener(c.emit)

	waitForCount(t, c, 1, 2*time.Second)

	// Idempotent no-ops: no phase advance → no publication.
	a.MarkUIRuntimeReady()
	a.MarkUIRuntimeReady()
	a.MarkUIReady()
	a.MarkUIReady()

	time.Sleep(80 * time.Millisecond)

	// The bootphase may legitimately advance twice here
	// (ui_runtime_ready → ui_ready), but every identical snapshot is
	// dropped: at most 2 events total (initial + ui_ready advance),
	// never a storm of 5.
	if n := c.count(); n > 2 {
		t.Fatalf("duplicate suppression failed: %d events for 1 real change", n)
	}
}

// TestSetStateListenerDoubleRegistrationRejected pins the composition
// contract: exactly one listener; a second registration is ignored
// (which also means it cannot leak a second publisher goroutine).
func TestSetStateListenerDoubleRegistrationRejected(t *testing.T) {
	a := newTestApp(t)
	defer a.Shutdown()

	first := newStateCollector()
	a.SetStateListener(first.emit)
	waitForCount(t, first, 1, 2*time.Second)

	second := newStateCollector()
	a.SetStateListener(second.emit)

	time.Sleep(80 * time.Millisecond)

	if n := second.count(); n != 0 {
		t.Fatalf("second listener received %d events; double registration must be ignored", n)
	}
}

// TestPublishersStopAtShutdown pins the shutdown guarantee: after
// App.Shutdown returns, no listener callback can run anymore.
func TestPublishersStopAtShutdown(t *testing.T) {
	a := newTestApp(t)

	c := newStateCollector()
	a.SetStateListener(c.emit)
	waitForCount(t, c, 1, 2*time.Second)

	a.Shutdown()

	// Drain any in-flight (already coalescing) emission, then force a
	// post-shutdown state change: nothing may be delivered.
	time.Sleep(120 * time.Millisecond)

	base := c.count()

	a.publishState()
	a.publishState()

	time.Sleep(120 * time.Millisecond)

	if n := c.count(); n != base {
		t.Fatalf("listener received %d events after Shutdown (base %d) — publisher survived shutdown", n, base)
	}

	// Double Shutdown stays safe (shutdownOnce idempotence with the
	// publisher stop inside).
	a.Shutdown()
}

// TestConnectionListenerReceivesSnapshots pins the connection bridge:
// SetConnectionListener subscribes to the manager's transition path
// and delivers snapshots — the initial one at registration, and the
// authoritative connection_failed snapshot after a failing connect
// (the test app has no cores; the failure is the machine's real
// terminal state, not an exception to it).
func TestConnectionListenerReceivesSnapshots(t *testing.T) {
	a := newTestApp(t)
	defer a.Shutdown()

	events := make(chan connection.Snapshot, 32)

	a.SetConnectionListener(func(s connection.Snapshot) {
		select {
		case events <- s:
		default:
		}
	})

	// Registration delivered the current (disconnected) snapshot.
	select {
	case s := <-events:
		if s.State != connection.StateDisconnected {
			t.Fatalf("initial connection snapshot = %q, want disconnected", s.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initial connection snapshot not delivered at registration")
	}

	// A real transition: an ad-hoc connect with NO available backend
	// traverses the state machine and fails it deterministically
	// (connection_failed) — the machine's real terminal state, not an
	// exception to it.
	svc := NewConnectionService(a)

	_, _ = svc.ConnectConfig(adHocTestConfig())

	deadline := time.After(3 * time.Second)

	for {
		select {
		case s := <-events:
			if s.State == connection.StateConnectionFailed {
				return
			}
		case <-deadline:
			t.Fatal("connection_failed snapshot not delivered after failing connect")
		}
	}
}
