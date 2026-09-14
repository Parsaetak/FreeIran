// bootphase_test.go verifies the unified startup state model (v0.9.4):
// phase ordering, the no-blocking-UI invariant (heavy work happens
// after ServicesReady, in Start), and the monotonic guard that makes
// late/stray phase signals unable to regress recorded state.
package app

import (
	"testing"
	"time"
)

// TestBootPhaseReachesServicesReadyWithoutStart proves the critical
// path: New() alone reaches services_ready — no scheduler, no
// ingestion, no cache warm-up is required for the service surface to
// exist (the UI binds here; everything else is background work).
func TestBootPhaseReachesServicesReadyWithoutStart(t *testing.T) {
	application := newTestApp(t)

	state := application.State()

	if state.BootPhase != BootServicesReady {
		t.Fatalf("boot phase = %q, want %q (Start never called)", state.BootPhase, BootServicesReady)
	}

	timings := application.BootTimings()

	for _, phase := range []string{BootBoot, BootWorkspaceReady, BootStoreReady, BootServicesReady} {
		ms, ok := timings[phase]
		if !ok {
			t.Fatalf("boot timing for %s missing", phase)
		}

		if ms < 0 {
			t.Fatalf("boot timing for %s negative: %d", phase, ms)
		}
	}

	// Phases after services_ready must NOT be recorded yet: they
	// belong to the UI runtime and background warm-up.
	for _, phase := range []string{BootUIRuntimeReady, BootUIReady, BootBackgroundWarm, BootReady} {
		if _, ok := timings[phase]; ok {
			t.Fatalf("phase %s recorded before Start(): blocking-UI regression", phase)
		}
	}
}

// TestBootPhaseMonotonicGuard proves late or stray phase signals can
// never move recorded state backwards (a stale "ui_ready" event after
// READY must be a no-op).
func TestBootPhaseMonotonicGuard(t *testing.T) {
	application := newTestApp(t)

	application.MarkUIRuntimeReady()

	if phase := application.State().BootPhase; phase != BootUIRuntimeReady {
		t.Fatalf("boot phase = %q, want %q", phase, BootUIRuntimeReady)
	}

	application.MarkUIReady()

	if phase := application.State().BootPhase; phase != BootUIReady {
		t.Fatalf("boot phase = %q, want %q", phase, BootUIReady)
	}

	// Stray duplicates and out-of-order signals: no-ops.
	application.MarkUIReady()
	application.MarkUIRuntimeReady()

	if phase := application.State().BootPhase; phase != BootUIReady {
		t.Fatalf("late ui_runtime_ready regressed state to %q", phase)
	}

	// Jump straight to the final phase; earlier markers stay no-ops.
	application.markBoot(BootReady)

	if phase := application.State().BootPhase; phase != BootReady {
		t.Fatalf("boot phase = %q, want %q", phase, BootReady)
	}

	application.MarkUIReady()

	if phase := application.State().BootPhase; phase != BootReady {
		t.Fatalf("late ui_ready regressed state to %q", phase)
	}
}

// TestBootPhaseBackgroundWarmupDoesNotBlockUI proves the §6/§7
// invariant end to end: Start() launches every expensive task in the
// background and the process reaches ready asynchronously, while
// State() was already consumable before any of it ran.
func TestBootPhaseBackgroundWarmupDoesNotBlockUI(t *testing.T) {
	application := newTestApp(t)

	// The UI would be usable exactly here: services bound, no warm-up
	// started. Observe the phase BEFORE Start to prove ordering.
	if phase := application.State().BootPhase; phase != BootServicesReady {
		t.Fatalf("phase before Start = %q, want services_ready", phase)
	}

	application.Start()

	// Start itself must not synchronously wait for warm-up completion:
	// it launches goroutines and returns. ready is recorded by the
	// warm-up goroutine; poll briefly for it.
	deadline := time.Now().Add(10 * time.Second)

	for {
		phase := application.State().BootPhase

		if phase == BootReady {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("boot phase = %q after 10s, want ready", phase)
		}

		time.Sleep(20 * time.Millisecond)
	}

	// The full telemetry table must now be present and time-ordered.
	timings := application.BootTimings()

	var last int64

	for _, phase := range []string{
		BootBoot, BootWorkspaceReady, BootStoreReady, BootServicesReady,
		BootBackgroundWarm, BootReady,
	} {
		ms, ok := timings[phase]
		if !ok {
			t.Fatalf("phase %s missing from telemetry", phase)
		}

		if ms < last {
			t.Fatalf("phase %s at %dms before earlier phase at %dms", phase, ms, last)
		}

		last = ms
	}
}
