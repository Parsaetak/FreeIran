// bootphase.go implements the unified startup state model (v0.9.4):
//
//	BOOT
//	 ↓
//	WORKSPACE_READY        (root resolved, directories ensured, migration done)
//	 ↓
//	STORE_METADATA_READY   (chunked store open, record count known)
//	 ↓
//	SERVICES_READY         (all engine services constructed; New returns)
//	 ↓
//	UI_RUNTIME_READY       (services bound to the frontend runtime)
//	 ↓
//	UI_READY               (the frontend reported its first usable frame)
//	 ↓
//	BACKGROUND_WARMUP      (scheduler, verification, cache warm-up running)
//	 ↓
//	READY                  (warm-up finished; full functionality)
//
// The invariant: nothing after SERVICES_READY may block UI readiness.
// Expensive work (storage verification, cache warming, core discovery,
// source refresh) is background-only and reported through State().
//
// Every transition records the elapsed wall-clock time since process
// boot, so the runtime log and diagnostics expose the real startup
// telemetry (phase → milliseconds) without any remote reporting.
package app

import (
	"sort"
	"time"
)

// Boot phases, in guaranteed transition order. A phase may be skipped
// only when the process fails before reaching it (fatal boot error).
const (
	BootBoot           = "boot"
	BootWorkspaceReady = "workspace_ready"
	BootStoreReady     = "store_metadata_ready"
	BootServicesReady  = "services_ready"
	BootUIRuntimeReady = "ui_runtime_ready"
	BootUIReady        = "ui_ready"
	BootBackgroundWarm = "background_warmup"
	BootReady          = "ready"
	BootShuttingDown   = "shutting_down"
)

// bootOrder assigns each phase its position in the lifecycle. It is
// the single authority for "did we already reach phase X" checks and
// for the monotonic-ordering regression test.
var bootOrder = map[string]int{
	BootBoot:           0,
	BootWorkspaceReady: 1,
	BootStoreReady:     2,
	BootServicesReady:  3,
	BootUIRuntimeReady: 4,
	BootUIReady:        5,
	BootBackgroundWarm: 6,
	BootReady:          7,
}

// bootPhaseRank returns the lifecycle position of a phase name, with
// unknown phases ordered after every known one (defensive: a future
// phase must never sort before READY in the ordering test).
func bootPhaseRank(phase string) int {
	if phase == "" {
		return -1 // no phase recorded yet: everything outranks it
	}

	if rank, ok := bootOrder[phase]; ok {
		return rank
	}

	return 1 << 30
}

// markBoot records a phase transition with its elapsed time. It is
// safe to call from any goroutine; the app mutex orders state writes
// and the atomic boot clock read needs no extra synchronization.
//
// Transitions are append-only and never go backwards: a markBoot call
// for a phase earlier than the current one is ignored (idempotent,
// late "ready" signals from stale listeners cannot regress state).
func (a *App) markBoot(phase string) {
	a.mu.Lock()

	if bootPhaseRank(phase) <= bootPhaseRank(a.state.BootPhase) {
		a.mu.Unlock()

		return
	}

	if a.bootTimings == nil {
		a.bootTimings = make(map[string]int64, 8)
	}

	a.bootTimings[phase] = time.Since(a.bootStart).Milliseconds()
	a.state.BootPhase = phase

	a.mu.Unlock()

	// A real boot-phase advance reaches the UI as a real event
	// (deduplicated by the publisher) — the BootProgress surface
	// updates the moment the engine moves forward.
	a.publishState()
}

// BootTimings returns a copy of the phase → elapsed-ms telemetry map.
// The map is small (≤ 8 entries) and copied so callers cannot mutate
// boot state.
func (a *App) BootTimings() map[string]int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()

	timings := make(map[string]int64, len(a.bootTimings))

	for phase, ms := range a.bootTimings {
		timings[phase] = ms
	}

	return timings
}

// bootTimingsSorted returns the recorded phases ordered by lifecycle
// position — used by the log line and the diagnostics service.
func (a *App) bootTimingsSorted() ([]string, []int64) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	phases := make([]string, 0, len(a.bootTimings))

	for phase := range a.bootTimings {
		phases = append(phases, phase)
	}

	sort.Slice(phases, func(i, j int) bool {
		return bootPhaseRank(phases[i]) < bootPhaseRank(phases[j])
	})

	values := make([]int64, len(phases))

	for i, phase := range phases {
		values[i] = a.bootTimings[phase]
	}

	return phases, values
}

// logBootTelemetry writes the phase → ms table to the runtime log in
// one structured line, e.g.
//
//	boot: workspace_ready=3ms store_metadata_ready=41ms …
//
// Developer/diagnostics-only: no remote telemetry exists anywhere.
func (a *App) logBootTelemetry() {
	if a.logger == nil {
		return
	}

	phases, values := a.bootTimingsSorted()
	line := "startup telemetry:"

	for i, phase := range phases {
		line += " " + phase + "=" + itoaMilli(values[i]) + "ms"
	}

	a.logger.Info("app", "boot_telemetry", "%s", line)
}

// itoaMilli formats an elapsed-milliseconds value without importing
// strconv for one call site (the file stays dependency-light).
func itoaMilli(ms int64) string {
	if ms == 0 {
		return "0"
	}

	negative := ms < 0
	if negative {
		ms = -ms
	}

	var digits [20]byte
	pos := len(digits)

	for ms > 0 {
		pos--
		digits[pos] = byte('0' + ms%10)
		ms /= 10
	}

	if negative {
		pos--
		digits[pos] = '-'
	}

	return string(digits[pos:])
}
