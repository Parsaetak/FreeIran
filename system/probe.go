// probe.go implements the synchronous, bounded child-command helper
// (RunProbe) for one-shot validations: provider binary version probes,
// smoke launches and any future "run this child, capture its output,
// guarantee it is gone" need.
//
// RunProbe routes the child through the SAME supervision pipeline as
// long-lived protocol cores (system.Start):
//
//   - no visible console window on Windows (CREATE_NO_WINDOW etc. —
//     the flags live ONLY in process_windows.go, never at call sites);
//   - Windows job-object binding (kill-on-close) or the supervised
//     process-tree fallback — either way no descendant can survive;
//   - Unix process-group supervision with the same guarantees;
//   - a bounded lifetime (caller-supplied timeout) and cancellation;
//   - deterministic, synchronizing cleanup (Stop joins the kill);
//   - bounded output capture for useful diagnostics.
//
// Provider packages must use this (or Start directly) instead of raw
// os/exec commands, so every child FreeIran ever creates shares one
// supervision and no-window contract.
package system

import (
	"context"
	"sync"
	"time"
)

// probeOutputCap bounds how much child output one probe captures
// (stdout + stderr combined). Version banners and smoke diagnostics
// are tiny; the cap keeps a pathological child from growing memory.
const probeOutputCap = 64 << 10

// probeStopGrace bounds the forced-termination join when a probe hits
// its deadline or the parent cancels: Stop's hard-kill phase is
// already deterministic, this only bounds the polite window.
const probeStopGrace = 3 * time.Second

// ProbeResult is the outcome of one bounded, supervised one-shot
// child run.
type ProbeResult struct {
	// State is the terminal ManagedProcess state (exited / stopped /
	// cancelled). StateExited with Launched=false means the launch
	// itself failed.
	State ProcessState

	// Launched reports whether the child was actually created.
	Launched bool

	// ExitCode is the child's exit code (-1 when unknown or when the
	// launch failed).
	ExitCode int

	// Output is the captured stdout+stderr (bounded, combined).
	Output string

	// Err is the process-level error: launch failure, cancellation
	// cause, or a non-zero exit status. Callers that tolerate
	// non-zero exits (version probes) inspect ExitCode/Output and
	// treat Err as diagnostics.
	Err error
}

// RunProbe runs one child command to completion under the full
// supervision pipeline and returns its captured output.
//
// The run is bounded by timeout (<= 0 selects a 15 s default) and by
// the parent context: whichever ends first kills the child through
// the supervised termination path (exec's context hook fires
// immediately; Stop synchronizes the deterministic cleanup). RunProbe
// never returns while the child (or any of its descendants) is still
// unsupervised.
//
// Result normalization (so callers can rely on exactly three
// outcomes):
//
//   - natural completion: State=exited, the real exit code, Err=nil
//     for exit 0 (non-zero exits carry the exit-status error —
//     callers that tolerate them, like version probes, inspect
//     ExitCode/Output);
//   - parent cancellation: State=cancelled, Err=the context cause —
//     including when the cancellation beat the launch itself;
//   - deadline: State=stopped or cancelled (whichever supervised
//     termination joined first), Err carries the deadline cause.
func RunProbe(ctx context.Context, spec ProcessSpec, timeout time.Duration) ProbeResult {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := &boundedBuffer{limit: probeOutputCap}

	spec.Stdout = out
	spec.Stderr = out

	proc, err := Start(runCtx, spec)
	if err != nil {
		res := ProbeResult{
			State:    StateExited,
			Launched: false,
			ExitCode: -1,
			Output:   out.String(),
			Err:      err,
		}

		// A launch failure driven by the parent's cancellation is a
		// cancellation, not a dependency failure.
		if cause := ctx.Err(); cause != nil {
			res.State = StateCancelled
			res.Err = cause
		}

		return res
	}

	// Wait for natural exit, the deadline, or parent cancellation.
	_ = proc.Wait(runCtx)

	// Deterministic join: if the context ended before the child was
	// reaped (kill in flight), Stop synchronizes the termination so
	// RunProbe can never return with a live unsupervised child.
	if proc.Running() {
		_ = proc.Stop(probeStopGrace)
	}

	// Reading the recorded result after the exited channel is closed
	// is race-free; a zero-timeout wait returns immediately.
	waitErr := proc.Wait(context.Background())

	res := ProbeResult{
		State:    proc.State(),
		Launched: true,
		ExitCode: proc.ExitCode(),
		Output:   out.String(),
		Err:      waitErr,
	}

	switch {
	case ctx.Err() != nil:
		// The caller gave up: the cancellation is the authoritative
		// outcome, whatever the join race classified internally.
		res.State = StateCancelled
		res.Err = ctx.Err()

	case runCtx.Err() != nil && (res.State == StateStopped || res.State == StateCancelled):
		// The probe's own deadline drove the termination. A natural
		// exit that won the boundary race keeps its real result.
		res.Err = runCtx.Err()
	}

	return res
}

// boundedBuffer is a concurrency-safe, size-capped byte sink. exec
// copies stdout and stderr through two concurrent goroutines, so the
// writer must be synchronized (same discipline as the test syncWriter).
// Bytes beyond the cap are counted but not stored.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	over  int
	limit int
}

// Write implements io.Writer.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()

	room := b.limit - len(b.buf)

	if room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.over += len(p) - room
		}
	} else {
		b.over += len(p)
	}

	b.mu.Unlock()

	return len(p), nil
}

// String returns the captured bytes under the same lock.
func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.buf)
}
