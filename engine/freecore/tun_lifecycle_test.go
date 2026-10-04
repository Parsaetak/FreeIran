package freecore

import (
	"context"
	"net"
	"testing"
	"time"
)

// flowsOnlyEngine builds an engine wired for TUN-flow serving (no
// listeners bound): the same shape the first-party TUN backend runs.
func flowsOnlyEngine(t *testing.T) *Engine {
	t.Helper()

	engine, err := NewEngine(Options{
		Route:     testRoute(),
		LocalHost: "127.0.0.1",
		LocalPort: freePort(t),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	return engine
}

// TestServeFlowsOnlyLifetimeCompletesOnlyAfterStop is the lifetime
// proof for the TUN serving mode:
//
//  1. ServeFlowsOnly starts → the completion signal CANNOT fire while
//     admission is open — even long before any flow exists (the
//     zero-counter Wait bug this release fixes);
//  2. accepted TUN flows are accounted for (the session registry);
//  3. stop closes admission: new flows are refused, not accepted into
//     a winding-down engine;
//  4. owned flows drain and completion fires exactly once.
func TestServeFlowsOnlyLifetimeCompletesOnlyAfterStop(t *testing.T) {
	engine := flowsOnlyEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := engine.ServeFlowsOnly(ctx); err != nil {
		t.Fatalf("ServeFlowsOnly: %v", err)
	}

	// (1) The engine must stay alive with ZERO flows: bounded negative
	// proof — if completion fires here, the monitor observed the
	// sentinel-less zero counter and the test fails deterministically.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	if err := engine.Wait(waitCtx); err == nil {
		waitCancel()
		t.Fatal("ServeFlowsOnly completed before its service lifetime ended (zero-counter Wait)")
	} else if waitCtx.Err() == nil {
		waitCancel()
		t.Fatalf("Wait returned %v before the wait deadline — completion fired while admission is open", err)
	} else {
		waitCancel()
	}

	// (2) One accepted TUN flow must be accounted for.
	client, server := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	flowDone := make(chan struct{})
	go func() {
		engine.HandleTUNFlow(context.Background(), server, "203.0.113.77:443")
		close(flowDone)
	}()

	// The flow dials a black-hole target through the default route's
	// dial timeout; while it is live the registry must show it.
	deadline := time.Now().Add(5 * time.Second)

	live := 0

	for time.Now().Before(deadline) {
		if live = engine.registry.Len(); live >= 1 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if live < 1 {
		t.Fatalf("live sessions = %d, want >= 1 after HandleTUNFlow", live)
	}

	// (3) Stop closes admission; a flow arriving afterwards is refused
	// (the conn closes immediately) instead of joining a dead engine.
	engine.Stop(0)

	refused := make(chan struct{})
	go func() {
		engine.HandleTUNFlow(context.Background(), client, "203.0.113.78:443")
		close(refused)
	}()

	select {
	case <-refused:
		// The refused call returns promptly: nothing keeps a reference.
	case <-time.After(5 * time.Second):
		t.Fatal("HandleTUNFlow after stop was admitted (admission lifetime leaked)")
	}

	// (4) Drain: end the accepted flow, then Wait completes.
	_ = client.Close()

	select {
	case <-flowDone:
	case <-time.After(10 * time.Second):
		t.Fatal("accepted TUN flow did not drain after stop")
	}

	waitCtx, waitCancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()

	if err := engine.Wait(waitCtx); err != nil {
		t.Fatalf("Wait after stop+drain: %v", err)
	}

	// Completion is exactly-once: repeated Waits observe the same
	// closed signal without re-firing anything.
	for i := 0; i < 3; i++ {
		if err := engine.Wait(waitCtx); err != nil {
			t.Fatalf("Wait repetition %d: %v", i, err)
		}
	}
}

// TestServeFlowsOnlyCancelClosesAdmission proves the parent-context
// cancellation path owns the same lifetime gate: cancel → admission
// closed → flows refused → drain → completion.
func TestServeFlowsOnlyCancelClosesAdmission(t *testing.T) {
	engine := flowsOnlyEngine(t)

	ctx, cancel := context.WithCancel(context.Background())

	if err := engine.ServeFlowsOnly(ctx); err != nil {
		t.Fatalf("ServeFlowsOnly: %v", err)
	}

	t.Cleanup(cancel)

	client, server := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	go engine.HandleTUNFlow(context.Background(), server, "203.0.113.79:443")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if engine.registry.Len() >= 1 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	// Cancel the PARENT context (the backend's teardown path).
	cancel()

	refused := make(chan struct{})
	go func() {
		engine.HandleTUNFlow(context.Background(), client, "203.0.113.80:443")
		close(refused)
	}()

	select {
	case <-refused:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleTUNFlow after parent cancel was admitted")
	}

	_ = client.Close()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()

	if err := engine.Wait(waitCtx); err != nil {
		t.Fatalf("Wait after cancel+drain: %v", err)
	}
}
