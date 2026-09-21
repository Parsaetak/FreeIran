// v0912_lifecycle_test.go — regression tests for the v0.9.12
// lifecycle closure (§18 of the release spec).
//
// The invariant class under test: a stale asynchronous event must
// NEVER mutate current lifecycle state. Concretely:
//
//   - bootstrap progress is monotonic within a run (a late lower-%
//     line cannot regress an observed 100%);
//   - the readiness verdict is immutable once published;
//   - events from a previous run (old scanner generation) are
//     discarded across stop/restart;
//   - events arriving after Stop are discarded (no resurrection);
//   - an accepting endpoint is EVIDENCE, never the Tor readiness
//     verdict — Start() may not report Ready without an observed
//     "Bootstrapped 100%".
//
// The restart-determinism test drives repeated start/stop cycles
// (the CI failure was timing-dependent: one successful pass proves
// little, so the cycle is repeated and every state assertion is
// exact).
package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fakeTorStallBin string

// buildFakeTorStall compiles the never-100% fixture once per test
// binary (same discipline as TestMain's faketor: a missing fixture is
// a hard failure).
func buildFakeTorStall(t *testing.T) string {
	t.Helper()

	if fakeTorStallBin != "" {
		return fakeTorStallBin
	}

	// The binary must outlive the test that built it (later runs
	// reuse the package-level cache), so it lives in its own temp
	// dir cleaned up with the test binary's exit, not with a single
	// test.
	shared, err := os.MkdirTemp("", "freeiran-faketorstall-*")
	if err != nil {
		t.Fatalf("stall fixture temp dir: %v", err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(shared) })

	bin := filepath.Join(shared, executableFileName("tor"))

	if out, err := exec.Command("go", "build", "-o", bin, "./testdata/faketorstall").CombinedOutput(); err != nil {
		t.Fatalf("build faketorstall: %v\n%s", err, out)
	}

	fakeTorStallBin = bin

	return bin
}

// plantTorBinary plants an executable as the engine's activated
// (installed) binary — the same discipline
// TestTorStartFailsFastOnBadBinary uses, so the engine never touches
// the network.
func plantTorBinary(t *testing.T, engine *TorEngine, binaryPath, version string) {
	t.Helper()

	if err := os.MkdirAll(engine.binary.BinDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(engine.binary.BinDir(), executableFileName("tor"))

	data, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}

	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := engine.binary.saveManifest(Manifest{
		Name:       TorName,
		BinaryPath: target,
		Version:    version,
		State:      string(StateInstalled),
	}); err != nil {
		t.Fatal(err)
	}
}

// ---- unit level: the run-state model itself ----------------------------

func TestTorBootstrapProgressIsMonotonic(t *testing.T) {
	engine := NewTorEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"), false)

	engine.mu.Lock()
	engine.runGen = 1
	engine.run.begin(1)
	engine.bootstrapReady = make(chan struct{})
	engine.mu.Unlock()

	ready := engine.bootstrapReadyCh()

	// Normal progression.
	engine.ingestBootstrapLine(1, "Jan 01 00:00:00.000 [notice] Bootstrapped 45% (Connecting): progress")
	engine.ingestBootstrapLine(1, "Jan 01 00:00:00.000 [notice] Bootstrapped 90% (Enabling): more progress")

	engine.mu.Lock()
	progress := engine.run.progress
	engine.mu.Unlock()

	if progress != 90 {
		t.Fatalf("progress = %d, want 90", progress)
	}

	// The defect class: a LATE lower-% line (the scanner received it
	// after a newer one) must never regress observed progress.
	engine.ingestBootstrapLine(1, "Jan 01 00:00:00.000 [notice] Bootstrapped 45% (Connecting): late line")

	engine.mu.Lock()
	progress = engine.run.progress
	engine.mu.Unlock()

	if progress != 90 {
		t.Fatalf("late lower-%% line regressed progress: %d, want 90", progress)
	}

	// 100% is observed by the scanner — its one authority.
	engine.ingestBootstrapLine(1, "Jan 01 00:00:00.000 [notice] Bootstrapped 100% (Done): Connected to the Tor network")

	select {
	case <-ready:
	default:
		t.Fatal("scanner observing 100% must close the readiness event")
	}

	// Even after 100%: a late 75% line changes nothing.
	engine.ingestBootstrapLine(1, "Jan 01 00:00:00.000 [notice] Bootstrapped 75% (Establishing): very late line")

	engine.mu.Lock()
	progress = engine.run.progress
	complete := engine.run.complete
	engine.mu.Unlock()

	if progress != 100 || complete {
		t.Fatalf("post-100%% state regressed: progress=%d complete=%v", progress, complete)
	}

	// The VERDICT belongs to the supervisor, not the scanner.
	engine.mu.Lock()
	engine.run.observeEndpointReady()
	engine.run.publishReady()
	verdict := engine.run.ready()
	engine.mu.Unlock()

	if !verdict {
		t.Fatal("supervisor verdict must publish")
	}
}

func TestTorStaleEventsNeverMutateCurrentRun(t *testing.T) {
	engine := NewTorEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"), false)

	engine.mu.Lock()
	engine.runGen = 1
	engine.run.begin(1)
	engine.mu.Unlock()

	// Run 1 reaches 100%.
	engine.ingestBootstrapLine(1, "Jan 01 00:00:00.000 [notice] Bootstrapped 100% (Done): Connected")

	// A future generation (impossible ordering) is dropped.
	engine.ingestBootstrapLine(2, "Jan 01 00:00:00.000 [notice] Bootstrapped 5% (Connecting): impossible")

	// The run ends (Stop / failed start).
	engine.mu.Lock()
	engine.run.end()
	engine.mu.Unlock()

	// An in-flight line that already entered the sink before the run
	// ended is still dropped — no resurrection.
	engine.ingestBootstrapLine(1, "Jan 01 00:00:00.000 [notice] Bootstrapped 60% (Establishing): in-flight")

	engine.mu.Lock()
	ended := engine.run.ended
	progress := engine.run.progress
	engine.mu.Unlock()

	if !ended || progress != 100 {
		t.Fatalf("stale event mutated ended run: ended=%v progress=%d", ended, progress)
	}

	// A NEW generation begins (restart); events from the OLD
	// generation must not touch it.
	engine.mu.Lock()
	engine.runGen = 2
	engine.run.begin(2)
	engine.mu.Unlock()

	engine.ingestBootstrapLine(1, "Jan 01 00:00:00.000 [notice] Bootstrapped 90% (Establishing): stale across restart")

	engine.mu.Lock()
	progress = engine.run.progress
	tag := engine.run.tag
	engine.mu.Unlock()

	if progress != 0 || tag != "" {
		t.Fatalf("stale event from run 1 mutated run 2: progress=%d tag=%q", progress, tag)
	}
}

// ---- integration level: the engine lifecycle ---------------------------

// TestTorLateLineAfterReadinessDoesNotRegress reproduces the exact
// CI failure mode deterministically: after Start() published READY,
// a "Bootstrapped 45%" line still arrives (the scanner is still
// draining). The pre-0.9.12 engine regressed ready → starting; the
// run-state model must keep the state AND the published bootstrap
// view immutable.
func TestTorLateLineAfterReadinessDoesNotRegress(t *testing.T) {
	engine := installFakeTorForTest(t)

	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if engine.State() != StateReady {
		t.Fatalf("state = %q, want ready", engine.State())
	}

	// Inject the late line through the LIVE scanner (the sink fires
	// synchronously — same path a real late stdout line takes).
	engine.mu.Lock()
	sc := engine.scanner
	engine.mu.Unlock()

	if sc == nil {
		t.Fatal("scanner must exist while running")
	}

	late := "Jan 01 00:00:00.000 [notice] Bootstrapped 45% (Connecting): late stdout line"

	if _, err := sc.Write([]byte(late + "\n")); err != nil {
		t.Fatalf("inject late line: %v", err)
	}

	if engine.State() != StateReady {
		t.Fatalf("late line regressed state: %q, want ready", engine.State())
	}

	info := engine.Info()

	if !info.Bootstrap.Complete || info.Bootstrap.Progress != 100 {
		t.Fatalf("late line regressed published bootstrap: %+v", info.Bootstrap)
	}

	// Stop must still be deterministic.
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if engine.State() != StateInstalled {
		t.Fatalf("post-stop state = %q", engine.State())
	}
}

// TestTorStaleIngestAcrossRestart fires events from a PREVIOUS run's
// generation into the CURRENT run — the in-flight-sink case a
// stop/restart boundary can produce. The new run must be untouched.
func TestTorStaleIngestAcrossRestart(t *testing.T) {
	engine := installFakeTorForTest(t)

	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	engine.mu.Lock()
	oldGen := engine.runGen
	engine.mu.Unlock()

	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}

	engine.mu.Lock()
	newGen := engine.runGen
	engine.mu.Unlock()

	if newGen != oldGen+1 {
		t.Fatalf("generation did not advance: %d -> %d", oldGen, newGen)
	}

	// Stale event from the previous run.
	engine.ingestBootstrapLine(oldGen, "Jan 01 00:00:00.000 [notice] Bootstrapped 45% (Connecting): stale")

	engine.mu.Lock()
	progress := engine.run.progress
	engine.mu.Unlock()

	if progress != 100 {
		t.Fatalf("stale event mutated the restarted run: progress=%d", progress)
	}

	if engine.State() != StateReady {
		t.Fatalf("state = %q, want ready after stale ingest", engine.State())
	}

	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("stop 2: %v", err)
	}
}

// TestTorRestartIsDeterministic repeats full start→ready→stop cycles.
// The CI failure ("restart state = starting") was timing-dependent —
// one pass proves little, so the cycle is REPEATED and every
// transition is asserted exactly.
func TestTorRestartIsDeterministic(t *testing.T) {
	engine := installFakeTorForTest(t)

	ctx := context.Background()

	for cycle := 1; cycle <= 3; cycle++ {
		if err := engine.Start(ctx); err != nil {
			t.Fatalf("cycle %d: start: %v", cycle, err)
		}

		// The published state must be READY immediately — never
		// starting, never flapping between the two.
		for i := 0; i < 50; i++ {
			if engine.State() != StateReady {
				t.Fatalf("cycle %d: state = %q right after start, want ready", cycle, engine.State())
			}

			time.Sleep(2 * time.Millisecond)
		}

		info := engine.Info()
		if !info.Bootstrap.Complete || info.Bootstrap.Progress != 100 {
			t.Fatalf("cycle %d: bootstrap = %+v", cycle, info.Bootstrap)
		}

		if err := engine.Stop(ctx); err != nil {
			t.Fatalf("cycle %d: stop: %v", cycle, err)
		}

		if engine.State() != StateInstalled {
			t.Fatalf("cycle %d: post-stop state = %q", cycle, engine.State())
		}

		if len(engine.Endpoints()) != 0 {
			t.Fatalf("cycle %d: endpoints must clear after stop", cycle)
		}
	}
}

// TestTorEndpointEvidenceIsNotReadiness pins the readiness CONTRACT:
// a Tor whose SOCKS endpoint accepts but which never emits
// "Bootstrapped 100%" must NOT become ready. The pre-0.9.12 engine
// returned Ready on the endpoint probe alone and manufactured
// Complete=true — the root defect of this release.
func TestTorEndpointEvidenceIsNotReadiness(t *testing.T) {
	engine := NewTorEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"), false)

	plantTorBinary(t, engine, buildFakeTorStall(t), "0.4.8.16")

	if err := engine.SetOptions(TorOptions{BootstrapTimeout: 900 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}

	err := engine.Start(context.Background())
	if err == nil {
		t.Fatal("start must NOT succeed without an observed Bootstrapped 100%")
	}

	if !strings.Contains(err.Error(), "bootstrap not observed") {
		t.Fatalf("error = %v, want bootstrap-deadline failure with evidence", err)
	}

	// Endpoint evidence must be visible and honest — the failure
	// message reports it, the published verdict stays false.
	if !strings.Contains(err.Error(), "socks endpoint accepting") {
		t.Fatalf("error = %v, want endpoint evidence recorded", err)
	}

	if engine.State() != StateFailed {
		t.Fatalf("state = %q, want failed", engine.State())
	}

	info := engine.Info()

	if info.Bootstrap.Complete {
		t.Fatalf("bootstrap verdict must stay false: %+v", info.Bootstrap)
	}

	if info.Bootstrap.Progress != 50 {
		t.Fatalf("bootstrap progress evidence = %d, want 50 (last observed)", info.Bootstrap.Progress)
	}

	if len(engine.Endpoints()) != 0 {
		t.Fatalf("endpoints must clear on failed start: %+v", engine.Endpoints())
	}
}

// installFakeTorForTest installs the standard faketor fixture
// through the engine's own install pipeline and returns the engine
// (installed but NOT started; the caller drives the lifecycle).
func installFakeTorForTest(t *testing.T) *TorEngine {
	t.Helper()

	dist := fakeDistServer(t, "15.0.20")

	root := t.TempDir()

	source := NewTorSource(testHTTPClient(dist.URL), false)
	source.BaseURL = dist.URL

	engine := NewTorEngineFromSource(root, testHTTPClient(dist.URL), source)

	if err := engine.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := engine.SetOptions(TorOptions{BootstrapTimeout: 30 * time.Second}); err != nil {
		t.Fatalf("options: %v", err)
	}

	return engine
}

// ---- psiphon: the same invariant class ---------------------------------

func TestPsiphonStaleEventsNeverMutateCurrentRun(t *testing.T) {
	engine := NewPsiphonEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"))

	engine.mu.Lock()
	engine.runGen = 1
	engine.run.begin(1)
	engine.mu.Unlock()

	// Tunnel-up evidence for run 1.
	engine.ingestTunnelLine(1, `client: tunnels are up`)

	engine.mu.Lock()
	progress := engine.run.progress
	engine.mu.Unlock()

	if progress != 100 {
		t.Fatalf("progress = %d, want 100", progress)
	}

	// The run ends.
	engine.mu.Lock()
	engine.run.end()
	engine.mu.Unlock()

	// An in-flight line must not resurrect anything.
	engine.ingestTunnelLine(1, `client: tunnels are up`)

	var ended, complete bool

	engine.mu.Lock()
	ended = engine.run.ended
	progress = engine.run.progress
	complete = engine.run.complete
	engine.mu.Unlock()

	if !ended || progress != 100 || complete {
		t.Fatalf("stale event mutated ended run: ended=%v progress=%d complete=%v", ended, progress, complete)
	}

	// A new run must not be touched by the old generation either.
	engine.mu.Lock()
	engine.runGen = 2
	engine.run.begin(2)
	engine.mu.Unlock()

	engine.ingestTunnelLine(1, `client: tunnels are up`)

	engine.mu.Lock()
	newProgress := engine.run.progress
	newComplete := engine.run.complete
	engine.mu.Unlock()

	if newProgress != 0 || newComplete {
		t.Fatalf("stale event mutated run 2: progress=%d complete=%v", newProgress, newComplete)
	}
}
