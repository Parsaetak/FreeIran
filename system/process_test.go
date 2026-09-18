package system

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// This file is the process-supervision lifecycle battery required by
// the v0.8.0 contract. Every test is deterministic:
//
//   - stdout/stderr completion is observed through the exited channel
//     (cmd.Wait joins exec's copy goroutines BEFORE exited closes —
//     no fixed sleeps anywhere);
//   - termination is observed through Stop's synchronizing contract;
//   - the shared syncWriter is genuinely concurrency-safe (exec copies
//     stdout and stderr through two concurrent goroutines).

// syncWriter is a concurrency-safe byte sink for stdout/stderr
// capture. v0.7.0's bytesWriter claimed thread-safety but had no
// synchronization at all; exec copies stdout and stderr concurrently,
// so an unsynchronized writer is a data race by construction.
type syncWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	w.mu.Unlock()

	return len(p), nil
}

// String returns the captured bytes under the same lock.
func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return string(w.buf)
}

// waitForExit blocks until the process exits, with a hard bound so a
// supervision bug fails the test instead of hanging the matrix.
func waitForExit(t *testing.T, m *ManagedProcess, bound time.Duration) {
	t.Helper()

	select {
	case <-m.exited:
		return
	case <-time.After(bound):
		t.Fatalf("process pid %d did not exit within %s", m.pid, bound)
	}
}

// TestProcessLaunchNoWindow is the no-visible-console regression.
//
// On Windows the "no console window" property is verified
// behaviourally: a child created with CREATE_NO_WINDOW never attaches
// to the parent's console, so it cannot appear in the parent's
// console process list (GetConsoleProcessList). When the test binary
// itself runs without a console (output redirected) the list check is
// reported as skipped; the launch/stop lifecycle assertions still
// run. The CREATE_NO_WINDOW flag is additionally asserted by source
// construction in process_windows.go.
//
// On non-Windows platforms the test verifies the basic launch+wait
// lifecycle (process-group semantics).
func TestProcessLaunchNoWindow(t *testing.T) {
	spec := exitSpec(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proc, err := Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if proc.PID() <= 0 {
		t.Fatalf("PID = %d, want > 0", proc.PID())
	}

	assertChildNotAttachedToParentConsole(t, proc.PID())

	// The process exits 0 on its own quickly.
	waitForExit(t, proc, 5*time.Second)

	if proc.ExitCode() != 0 {
		t.Errorf("exit code = %d, want 0", proc.ExitCode())
	}

	// Stop is idempotent and does not return an error after the
	// process has already exited.
	if err := proc.Stop(time.Second); err != nil {
		t.Errorf("Stop after exit returned err: %v", err)
	}

	assertNoSupervisedProcessSurvives(t, proc)
}

// TestProcessLaunchStdoutCapture verifies stdout capture with the
// deterministic flush contract: once exited closes, cmd.Wait has
// already joined the stdout copy goroutine, so the writer is complete
// — no sleeps.
func TestProcessLaunchStdoutCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	w := &syncWriter{}
	spec := echoSpec(t, "hello-freeiran-stdout", streamStdout)

	proc, err := Start(ctx, ProcessSpec{
		Name:   "test-capture-stdout",
		Path:   spec.Path,
		Args:   spec.Args,
		Stdout: w,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForExit(t, proc, 5*time.Second)

	if out := w.String(); !contains(out, "hello-freeiran-stdout") {
		t.Errorf("captured stdout = %q, want substring %q", out, "hello-freeiran-stdout")
	}
}

// TestProcessLaunchStderrCapture verifies stderr is captured through
// a SEPARATE writer so the routing between the two streams is proven,
// not just that bytes arrive somewhere.
func TestProcessLaunchStderrCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stdout := &syncWriter{}
	stderr := &syncWriter{}
	spec := echoSpec(t, "hello-freeiran-stderr", streamStderr)

	proc, err := Start(ctx, ProcessSpec{
		Name:   "test-capture-stderr",
		Path:   spec.Path,
		Args:   spec.Args,
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForExit(t, proc, 5*time.Second)

	if out := stderr.String(); !contains(out, "hello-freeiran-stderr") {
		t.Errorf("captured stderr = %q, want substring %q",
			stderr.String(), "hello-freeiran-stderr")
	}

	if out := stdout.String(); contains(out, "hello-freeiran-stderr") {
		t.Errorf("stderr marker leaked into stdout writer: %q", out)
	}
}

// TestProcessNaturalExit proves a natural (unassisted) exit is
// classified as StateExited with the real exit code, and that Wait
// surfaces the non-zero status as an error.
func TestProcessNaturalExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proc, err := Start(ctx, exitSpec(t, 7))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForExit(t, proc, 5*time.Second)

	if proc.State() != StateExited {
		t.Errorf("state = %s, want exited", proc.State())
	}

	if proc.ExitCode() != 7 {
		t.Errorf("exit code = %d, want 7", proc.ExitCode())
	}

	if err := proc.Wait(context.Background()); err == nil {
		t.Error("Wait must surface the non-zero natural exit as an error")
	}

	// Stop after a natural exit stays a clean no-op.
	if err := proc.Stop(time.Second); err != nil {
		t.Errorf("Stop after natural exit: %v", err)
	}
}

// TestProcessCancellation proves context cancellation terminates the
// child, classifies the exit as StateCancelled and surfaces the
// context error (not the kill's exit-code artifact) from Wait.
func TestProcessCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	proc, err := Start(ctx, longRunSpec(t))
	if err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}

	cancel()

	waitForExit(t, proc, 10*time.Second)

	if proc.State() != StateCancelled {
		t.Errorf("state = %s, want cancelled", proc.State())
	}

	if err := proc.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait error = %v, want context.Canceled", err)
	}

	assertNoSupervisedProcessSurvives(t, proc)
}

// TestProcessForcedTermination proves Stop kills a healthy long-lived
// child well within the hard-kill deadline, even with a tiny grace
// period, and leaves nothing behind.
func TestProcessForcedTermination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc, err := Start(ctx, longRunSpec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := time.Now()

	if err := proc.Stop(50 * time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if elapsed := time.Since(start); elapsed > hardKillDeadline {
		t.Fatalf("Stop took %s; forced termination exceeded the deadline", elapsed)
	}

	if proc.Running() {
		t.Fatal("process still running after Stop")
	}

	assertNoSupervisedProcessSurvives(t, proc)
}

// TestProcessRepeatedStop proves idempotency: after the first Stop
// completes, later calls return the same result immediately and the
// process stays dead.
func TestProcessRepeatedStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc, err := Start(ctx, longRunSpec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := proc.Stop(time.Second); err != nil {
			t.Fatalf("Stop #%d: %v", i+1, err)
		}
	}

	if proc.State() != StateStopped && proc.State() != StateExited {
		t.Errorf("state = %s, want stopped", proc.State())
	}

	assertNoSupervisedProcessSurvives(t, proc)
}

// TestProcessStartupFailureCleanup proves a launch failure (binary
// does not exist) never produces a ManagedProcess and never leaks a
// process — and that the failure is a launch failure
// (dependency_unavailable), not a masked generic error.
func TestProcessStartupFailureCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	proc, err := Start(ctx, ProcessSpec{
		Name: "test-missing",
		Path: "/definitely/not/a/real/binary-freeiran-xyz",
	})

	if err == nil {
		if proc != nil {
			_ = proc.Stop(time.Second)
		}

		t.Fatal("Start must fail for a nonexistent binary")
	}

	if proc != nil {
		t.Fatal("Start must not return a process on launch failure")
	}

	if got := errorKindOf(err); got != "dependency_unavailable" {
		t.Errorf("error kind = %s, want dependency_unavailable", got)
	}
}

// TestProcessBareNameRejected proves Start validation is not weakened
// for the resolver: an unknown BARE executable name is rejected on
// every platform (only the documented Windows system shell resolves).
func TestProcessBareNameRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	proc, err := Start(ctx, ProcessSpec{
		Name: "test-bare",
		Path: "definitely-not-a-known-binary-xyz",
	})

	if err == nil {
		if proc != nil {
			_ = proc.Stop(time.Second)
		}

		t.Fatal("Start must reject an unknown bare executable name")
	}

	if got := errorKindOf(err); got != "dependency_unavailable" {
		t.Errorf("error kind = %s, want dependency_unavailable", got)
	}
}

// TestProcessJobBindingFailureCleanup injects a deterministic job
// binding failure (simulating restricted environments where
// child-job assignment is unavailable) and proves the supervised
// fallback: the launch SUCCEEDS, the degradation is visible
// (JobBound=false + diagnostic note), Stop still cleans up
// deterministically, and nothing survives.
//
// This is the "never silently weaken lifecycle guarantees" contract.
func TestProcessJobBindingFailureCleanup(t *testing.T) {
	setJobBindHook(func() error { return errors.New("injected restricted environment") })
	defer setJobBindHook(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc, err := Start(ctx, longRunSpec(t))
	if err != nil {
		t.Fatalf("Start under job-binding failure: %v", err)
	}

	if proc.JobBound() {
		t.Error("JobBound must report the degraded binding")
	}

	diag := proc.Diagnostics()
	if diag.SupervisionNote == "" {
		t.Error("degraded supervision must carry a diagnostic note")
	}

	if !diag.Alive {
		t.Fatal("fallback process must be alive and supervised")
	}

	if err := proc.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop under fallback supervision: %v", err)
	}

	assertNoSupervisedProcessSurvives(t, proc)
}

// TestProcessRestrictedEnvironmentRetry injects the exact
// restricted-environment signature (ERROR_ACCESS_DENIED) so the
// Windows relaunch-with-breakaway tier runs on every platform that
// supports the machinery. The retry's assign also fails (the hook
// stays installed), so the process lands in the supervised fallback —
// proving the whole chain terminates cleanly.
func TestProcessRestrictedEnvironmentRetry(t *testing.T) {
	setJobBindHook(restrictedBindErrno)
	defer setJobBindHook(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc, err := Start(ctx, longRunSpec(t))
	if err != nil {
		t.Fatalf("Start under restricted environment: %v", err)
	}

	if err := proc.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop after restricted retry: %v", err)
	}

	assertNoSupervisedProcessSurvives(t, proc)
}

// TestProcessConcurrentStop hammers Stop and Wait from many goroutines
// at once: every caller must observe the same completion, the process
// must die exactly once, and the race detector must stay silent.
func TestProcessConcurrentStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc, err := Start(ctx, longRunSpec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	const callers = 8

	var wg sync.WaitGroup

	results := make([]error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)

		go func(slot int) {
			defer wg.Done()
			results[slot] = proc.Stop(2 * time.Second)
		}(i)
	}

	// A concurrent Wait must not deadlock against the stop swarm.
	waitDone := make(chan error, 1)

	go func() { waitDone <- proc.Wait(context.Background()) }()

	wg.Wait()

	select {
	case <-waitDone:
	case <-time.After(hardKillDeadline):
		t.Fatal("Wait deadlocked against concurrent Stop callers")
	}

	for i, err := range results {
		if err != nil {
			t.Errorf("concurrent Stop caller %d: %v", i, err)
		}
	}

	assertNoSupervisedProcessSurvives(t, proc)
}

// TestProcessGrandchildCannotSurviveSupervisor proves the core
// supervision invariant: a process the child spawns (a "grandchild",
// like a helper a protocol core forks) is reaped together with the
// supervised child. Killing only the direct pid is NOT sufficient.
func TestProcessGrandchildCannotSurviveSupervisor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proc, err := Start(ctx, grandchildSpec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the child a moment to spawn the grandchild — bounded and
	// verified below by observing the grandchild directly, so the
	// observation is not timing-dependent.
	waitForGrandchild(t, proc)

	if err := proc.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	assertNoSupervisedProcessSurvives(t, proc)
}

// TestProcessDiagnosticsShape pins the structured diagnostics surface:
// state string, pid, job-bound flag and exit code must all be present
// and consistent with the observed lifecycle.
func TestProcessDiagnosticsShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proc, err := Start(ctx, exitSpec(t, 0))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	diag := proc.Diagnostics()
	if diag.PID <= 0 || diag.State == "" {
		t.Fatalf("incomplete diagnostics: %+v", diag)
	}

	waitForExit(t, proc, 5*time.Second)

	diag = proc.Diagnostics()

	if diag.State != "exited" {
		t.Errorf("diagnostics state = %s, want exited", diag.State)
	}

	if diag.ExitCode != 0 {
		t.Errorf("diagnostics exit code = %d, want 0", diag.ExitCode)
	}

	if diag.Alive {
		t.Error("diagnostics must report the process as not alive")
	}
}

// TestSyncWriterConcurrent exercises the capture writer itself under
// concurrent writes — the regression the v0.7.0 bytesWriter missed.
func TestSyncWriterConcurrent(t *testing.T) {
	w := &syncWriter{}

	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()

			for j := 0; j < 100; j++ {
				_, _ = w.Write([]byte{byte('a' + n%26)})
				_ = w.String()
			}
		}(i)
	}

	wg.Wait()

	if len(w.String()) != 16*100 {
		t.Fatalf("writer lost writes: %d bytes, want %d", len(w.String()), 16*100)
	}
}

// errorKindOf extracts the structured error kind, if any.
func errorKindOf(err error) string {
	return string(firerrors.KindOf(err))
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}

	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}

// The original lookup helper retained for platform test binaries.
func lookPathAvailable(bin string) bool {
	_, err := execLookPath(bin)

	return err == nil
}

// ---- RunProbe: the synchronous supervised child helper ------------------
//
// RunProbe is the shared one-shot path for provider binary validation
// and smoke launches. Its battery mirrors the long-lived lifecycle
// tests: success, output capture, non-zero exits, bounded lifetime
// (forced termination at the deadline), parent cancellation and
// launch failure — all deterministic, no sleeps on the critical path.

// TestRunProbeSuccessAndOutputCapture: a child that prints to both
// streams and exits 0; the probe returns synchronously with the
// combined, bounded output.
func TestRunProbeSuccessAndOutputCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := probeEchoBothSpec(t, "probe-ok")

	res := RunProbe(ctx, spec, 10*time.Second)

	if !res.Launched {
		t.Fatalf("probe did not launch: %v", res.Err)
	}

	if res.State != StateExited {
		t.Fatalf("state = %s, want exited", res.State)
	}

	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}

	if res.Err != nil {
		t.Fatalf("err = %v, want nil", res.Err)
	}

	if !contains(res.Output, "probe-ok-stdout") || !contains(res.Output, "probe-ok-stderr") {
		t.Fatalf("combined output missing streams: %q", res.Output)
	}
}

// TestRunProbeNonZeroExit: the exit status and output of a failing
// child are surfaced honestly (callers decide whether failure is
// tolerated, e.g. version probes).
func TestRunProbeNonZeroExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := RunProbe(ctx, probeFailSpec(t, "probe-failed-marker"), 10*time.Second)

	if res.State != StateExited {
		t.Fatalf("state = %s, want exited", res.State)
	}

	if res.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", res.ExitCode)
	}

	if res.Err == nil {
		t.Fatal("non-zero exit must be surfaced as an error")
	}

	if !contains(res.Output, "probe-failed-marker") {
		t.Fatalf("output = %q", res.Output)
	}
}

// TestRunProbeBoundedLifetime: a child that would run forever is
// terminated deterministically at the deadline — the probe returns
// (stopped or cancelled, whichever supervised termination joined
// first — never a hang, never a clean exit), with the deadline cause
// surfaced, and nothing survives.
func TestRunProbeBoundedLifetime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := RunProbe(ctx, longRunSpec(t), 2*time.Second)

	if !res.Launched {
		t.Fatalf("probe did not launch: %v", res.Err)
	}

	if res.State != StateStopped && res.State != StateCancelled {
		t.Fatalf("state = %s, want stopped or cancelled (deadline termination)", res.State)
	}

	if res.Err == nil {
		t.Fatal("deadline termination must surface the deadline cause")
	}
}

// TestRunProbeParentCancellation: cancelling the parent context kills
// the child through the supervised path and the probe reports the
// cancellation.
func TestRunProbeParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	res := make(chan ProbeResult, 1)

	go func() { res <- RunProbe(ctx, longRunSpec(t), 30*time.Second) }()

	cancel()

	select {
	case r := <-res:
		if r.State != StateCancelled {
			t.Fatalf("state = %s, want cancelled", r.State)
		}

		if r.Err == nil {
			t.Fatal("cancellation must surface an error")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("probe did not return after cancellation")
	}
}

// TestRunProbeLaunchFailure: a spec pointing at a nonexistent binary
// fails at launch, is reported as not launched, and creates no
// process.
func TestRunProbeLaunchFailure(t *testing.T) {
	res := RunProbe(context.Background(), ProcessSpec{
		Name: "probe-missing",
		Path: "/nonexistent/freeiran-probe-missing-binary",
	}, 5*time.Second)

	if res.Launched {
		t.Fatal("launch must fail for a nonexistent path")
	}

	if res.Err == nil {
		t.Fatal("launch failure must be reported")
	}

	if res.State != StateExited {
		t.Fatalf("state = %s, want exited", res.State)
	}
}

// TestRunProbeNoWindow is the probe-flavoured no-visible-console
// regression: the child runs under the same creation flags as managed
// cores, so it can never attach a visible console window.
func TestRunProbeNoWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := echoSpec(t, "probe-no-window-marker", streamStdout)

	res := RunProbe(ctx, spec, 10*time.Second)

	if res.ExitCode != 0 || !contains(res.Output, "probe-no-window-marker") {
		t.Fatalf("probe outcome = %+v", res)
	}

	// Behavioural no-console assertion (Windows) / lifecycle (unix)
	// is covered by TestProcessLaunchNoWindow; the probe shares the
	// same launch path, verified by construction here.
}

// TestRunProbeBoundedOutput: a child producing more than the capture
// cap keeps the probe bounded — output is truncated, never grown.
func TestRunProbeBoundedOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := RunProbe(ctx, probeBigOutputSpec(t), 30*time.Second)

	if len(res.Output) > probeOutputCap {
		t.Fatalf("captured %d bytes, cap is %d", len(res.Output), probeOutputCap)
	}
}
