package connection

import (
	"context"
	"testing"
	"time"
)

// generation_internal_test.go (package connection — white-box) pins the
// generation guard's entry contract directly: a recheck carrying a
// generation that is not the manager's current session epoch never
// runs, never mutates state, never tears anything down.

func TestRunStabilityRecheckStaleGenerationAppliesNothing(t *testing.T) {
	m := New(Options{
		Verify:                 VerifyPolicy{Target: "http://127.0.0.1:1/", Timeout: 2 * time.Second},
		VerifyInterval:         time.Millisecond,
		VerifyGrace:            time.Millisecond,
		VerifyFailureThreshold: 1,
	})

	m.mu.Lock()
	m.state = StateConnectedVerified
	m.verifiedAt = time.Now().UTC()
	m.nextVerifyAt = time.Now().Add(-time.Hour) // due right now
	m.verifyFailures = 0                        // threshold 1: one failure tears down
	m.mu.Unlock()

	// A stale generation (999) is not the manager's epoch (0): the
	// recheck must return without probing or mutating anything.
	m.runStabilityRecheck(context.Background(), "127.0.0.1:1", 999)

	m.mu.Lock()
	state, failures, lastVerify := m.state, m.verifyFailures, m.lastVerify
	m.mu.Unlock()

	if state != StateConnectedVerified {
		t.Fatalf("state = %s, want connected_verified (stale recheck mutated state)", state)
	}

	if failures != 0 {
		t.Fatalf("verifyFailures = %d, want 0 (stale recheck recorded a failure)", failures)
	}

	if lastVerify.OK || lastVerify.FailureClass != "" {
		t.Fatalf("stale recheck applied a verification result: %+v", lastVerify)
	}
}

func TestFailAtStaleGenerationLeavesStateUntouched(t *testing.T) {
	m := New(Options{})

	m.mu.Lock()
	m.state = StateDisconnected
	m.lastError = "original"
	m.mu.Unlock()

	// A stale session's failure must not clobber the current state.
	_, _ = m.failAt(42, errTestFake("stale failure"))

	m.mu.Lock()
	state, lastErr := m.state, m.lastError
	m.mu.Unlock()

	if state != StateDisconnected {
		t.Fatalf("state = %s, want disconnected (stale failure clobbered state)", state)
	}

	if lastErr != "original" {
		t.Fatalf("lastError = %q, want the original error preserved", lastErr)
	}

	// The current generation (0) records normally.
	_, _ = m.failAt(0, errTestFake("fresh failure"))

	if m.State() != StateConnectionFailed {
		t.Fatalf("state = %s, want connection_failed for the current generation", m.State())
	}
}

type errTestFake string

func (e errTestFake) Error() string { return string(e) }
