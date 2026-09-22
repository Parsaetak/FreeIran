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

	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/internal/logging"
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

	// v0.9.13: AppState.BootTimings was declared and compared by
	// the publisher but never populated — diagnostics/state (and
	// the UI boot telemetry surface) never received the timings.
	// Keep the state mirror in sync with the authoritative map.
	if a.state.BootTimings == nil {
		a.state.BootTimings = make(map[string]int64, len(bootOrder)+1)
	}

	a.state.BootTimings[phase] = a.bootTimings[phase]

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

// logBootTelemetry writes the phase → ms table to the runtime log as
// ONE structured record. v0.9.13: the record is debug-severity and
// lifecycle-tagged, so the Normal profile no longer carries a
// standalone boot_telemetry line — the full timing table stays
// available to Detailed/Debug profiles and to diagnostics through
// AppState.BootTimings. The timings ride the structured fields only;
// the message never repeats them.
//
// Developer/diagnostics-only: no remote telemetry exists anywhere.
func (a *App) logBootTelemetry() {
	if a.logger == nil {
		return
	}

	phases, values := a.bootTimingsSorted()
	timings := make(map[string]any, len(phases))

	for i, phase := range phases {
		timings[phase] = values[i]
	}

	a.logger.Log(logging.Record{
		Level:      logging.LevelDebug,
		Subsystem:  "app",
		Event:      "boot_telemetry",
		Message:    "startup phase timings recorded",
		Status:     "diagnostic",
		Lifecycle:  true,
		DurationMS: a.bootTimings[BootReady],
		Fields:     map[string]any{"timings": timings},
	})
}

// logWarmupComplete emits the single compact Normal-profile record
// that reports background warm-up completion (v0.9.13). Every value
// is measured: the warmup duration is the real elapsed time between
// the background_warmup and ready boot phases, and the core count is
// the live registry snapshot — nothing is estimated or invented.
func (a *App) logWarmupComplete() {
	if a.logger == nil {
		return
	}

	phases, values := a.bootTimingsSorted()

	var warmupStartMS, readyMS int64

	for i, phase := range phases {
		switch phase {
		case BootBackgroundWarm:
			warmupStartMS = values[i]
		case BootReady:
			readyMS = values[i]
		}
	}

	warmupMS := readyMS - warmupStartMS
	if warmupMS < 0 {
		warmupMS = 0
	}

	cores := 0
	if a.coreRegistry != nil {
		for _, backend := range a.coreRegistry.Backends() {
			if backend.Status == core.StatusAvailable {
				cores++
			}
		}
	}

	a.logger.Log(logging.Record{
		Level:      logging.LevelInfo,
		Subsystem:  "app",
		Event:      "warmup_complete",
		Message:    "background warmup complete",
		Status:     "ready",
		DurationMS: warmupMS,
		Fields: map[string]any{
			"warmup_ms": warmupMS,
			"cores":     cores,
		},
	})
}
