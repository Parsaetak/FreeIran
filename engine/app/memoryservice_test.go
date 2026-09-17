package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/booster"
	"github.com/Parsaetak/FreeIran/engine/mempressure"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// TestMemoryServiceWiredOnBoot verifies the Memory Booster 2.0 wiring:
// the app boots with the controller attached, the sampler produces
// real measurements, and the diagnostics surface carries the full
// pressure + settings picture.
func TestMemoryServiceWiredOnBoot(t *testing.T) {
	app := newTestApp(t)

	if app.memory == nil {
		t.Fatal("memory service must be wired at boot")
	}

	// One manual sample must complete without panicking on any
	// lazily-created subsystem (queue not yet initialized).
	app.memory.sample()

	snap := app.memory.Snapshot()

	if snap.Pressure.State != mempressure.StateNormal {
		t.Errorf("fresh app pressure = %s, want normal", snap.Pressure.State)
	}

	if snap.Booster.QueueConcurrency <= 0 {
		t.Errorf("booster queue concurrency = %d, want > 0", snap.Booster.QueueConcurrency)
	}

	if snap.CacheBytes < 0 || snap.QueueBytes < 0 || snap.PendingWAL < 0 {
		t.Errorf("negative memory measurements: %+v", snap)
	}

	// The diagnostics service exposes the same snapshot.
	diag := NewDiagnosticsService(app)
	if diag.Memory().Pressure.State != snap.Pressure.State {
		t.Error("DiagnosticsService.Memory diverges from the service snapshot")
	}
}

// TestMemoryServicePressureShedsQueueWorkers drives the controller
// through a synthetic pressure spike (queue-memory reporter set far
// above its ceiling) and verifies the adaptive chain end to end:
// state transition, booster floor settings, and queue worker
// reduction through SetConcurrency.
func TestMemoryServicePressureShedsQueueWorkers(t *testing.T) {
	app := newTestApp(t)

	// Create the queue (lazy init) with a known worker count.
	svc := NewTestQueueService(app)
	if _, err := svc.ensureQueue(); err != nil {
		t.Fatalf("ensureQueue: %v", err)
	}

	before := queueCurrentConcurrency(app)
	if before < 2 {
		t.Skipf("initial worker count %d too small for the shed assertion", before)
	}

	// Inject critical pressure: queue bytes far above the ceiling.
	app.memory.pressure.SetQueueBytes(2 * mempressure.DefaultCeiling().QueueBytes)

	snap := app.memory.pressure.Sample()
	if snap.State != mempressure.StateCritical {
		t.Fatalf("pressure state = %s, want critical after injection", snap.State)
	}

	// The booster must drop to the floors on its next tick.
	limits := booster.DefaultLimits()
	settings := app.memory.boost.Tick()

	if settings.QueueConcurrency != limits.QueueConcurrencyMin {
		t.Fatalf("booster queue concurrency under critical pressure = %d, want floor %d",
			settings.QueueConcurrency, limits.QueueConcurrencyMin)
	}

	// The applied settings reach the live queue through the
	// OnChange listener.
	deadline := time.Now().Add(5 * time.Second)

	for queueCurrentConcurrency(app) > settings.QueueConcurrency &&
		time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if after := queueCurrentConcurrency(app); after > settings.QueueConcurrency {
		t.Fatalf("queue workers after pressure = %d, want <= %d",
			after, settings.QueueConcurrency)
	}

	// Recovery: report comfortable memory, tick back up gradually.
	app.memory.pressure.SetQueueBytes(0)
	_ = app.memory.pressure.Sample()

	if state := app.memory.pressure.State(); state != mempressure.StateNormal &&
		state != mempressure.StateElevated {
		t.Fatalf("pressure after recovery = %s, want normal/elevated", state)
	}
}

// queueCurrentConcurrency reads the live worker-pool target.
func queueCurrentConcurrency(app *App) int {
	if app.testQueue == nil {
		return 0
	}

	return int(app.testQueue.DesiredWorkers())
}

// TestMemoryServiceStartStopLifecycle verifies the sampler goroutine
// starts with App.Start, terminates promptly with Shutdown, and a
// sample pass completes in between — the exact sequence the headless
// smoke test (cmd/freeiran --smoke-test) exercises on Windows.
func TestMemoryServiceStartStopLifecycle(t *testing.T) {
	app := newTestApp(t)

	app.Start()

	// The sampler is running; drive one manual sample to prove the
	// loop body works with every subsystem present.
	app.memory.sample()

	if app.memory.Snapshot().Samples < 1 {
		t.Error("manual sample must be recorded")
	}

	// Shutdown stops the memory controller first; the whole app
	// shutdown must complete well inside the test timeout (a stuck
	// sampler goroutine would hang Shutdown).
	done := make(chan struct{})

	go func() {
		app.Shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown blocked on the memory controller")
	}
}

// TestRoutineMemoryAdjustLoggingSuppressed is the v0.9.5 regression
// guard: routine booster adjustments must NOT produce "memory_adjust"
// INFO entries (they fired on every 5-second tick under oscillating
// load), while genuine warnings — pressure transitions — MUST still
// be logged. The suppression happens at the producer, not by hiding
// INFO globally, so other informational events keep flowing.
func TestRoutineMemoryAdjustLoggingSuppressed(t *testing.T) {
	logDir := t.TempDir()

	lg, err := logging.Open(logging.Options{Dir: logDir, Name: "memtest.log"})
	if err != nil {
		t.Fatalf("open logger: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	entries, stop := lg.Subscribe(512)
	t.Cleanup(stop)

	app, err := New(Options{
		BaseDir:             filepath.Join(t.TempDir(), "freeiran"),
		Logger:              lg,
		RefreshInterval:     time.Hour,
		RunIngestionOnStart: false,
		SkipDefaultSources:  true,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(app.Shutdown)

	// 1. Routine booster adjustment: a deep backlog grows the worker
	// pool, firing OnChange — the removed "memory_adjust" producer.
	inputs := app.memory.boost.Inputs()
	inputs.QueueBacklog.Store(1 << 20) // deep backlog
	app.memory.boost.Tick()
	app.memory.boost.Tick()

	// 2. A REAL memory-pressure transition must still log a warning.
	// (Set the reporter input directly: memoryservice.sample() would
	// overwrite it with the live subsystem measurements, which are all
	// small in a fresh test app.)
	app.memory.pressure.SetQueueBytes(1 << 40) // far above any ceiling
	_ = app.memory.pressure.Sample()

	// Collect entries for a bounded window.
	collect := func(d time.Duration) []logging.Entry {
		var got []logging.Entry

		deadline := time.After(d)

		for {
			select {
			case e := <-entries:
				got = append(got, e)
			case <-deadline:
				return got
			}
		}
	}

	seen := collect(300 * time.Millisecond)

	for _, e := range seen {
		if e.Event == "memory_adjust" {
			t.Fatalf("routine memory_adjust entry survived: %+v", e)
		}
	}

	var sawPressureWarn bool

	for _, e := range seen {
		if e.Event == "memory_pressure" && e.Level == logging.LevelWarn {
			sawPressureWarn = true
		}
	}

	if !sawPressureWarn {
		t.Fatalf("pressure warning missing after transition (entries: %d)", len(seen))
	}
}
