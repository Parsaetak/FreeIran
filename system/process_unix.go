//go:build !windows

package system

import (
	"context"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

func execLookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func executableName(name string) string {
	return name
}

func platformNote() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// concealChild is the non-Windows counterpart of the hidden-console
// helper: there is no console-window concept to suppress, so it is a
// deliberate no-op (the symbol exists so call sites stay
// platform-neutral).
func concealChild(cmd *exec.Cmd) {
	_ = cmd
}

// launchProcess starts a managed process with stdout/stderr captured.
// The child runs in its own process group (setpgid) so a forced group
// kill cannot leak grandchildren: everything the core spawns inherits
// the group, and Stop signals the group, then SIGKILLs it.
//
// Output capture: when ProcessSpec carries Stdout/Stderr writers the
// child's streams are connected to them (exec creates the pipes and
// copies stdout and stderr through two concurrent goroutines); cmd
// waits for both copy goroutines, so the writers are complete once
// the exited channel closes — the deterministic flush contract the
// tests rely on (no sleeps).
//
// Job binding: the jobHandle placeholder honours the same binding
// protocol as Windows (including the test failure-injection hook) so
// the supervised-fallback lifecycle is exercised uniformly; the actual
// kernel guarantee on Unix is the process group itself.
func launchProcess(ctx context.Context, spec ProcessSpec, cancelLaunch context.CancelFunc) (*ManagedProcess, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.WorkDir
	cmd.Stdout = orDiscard(spec.Stdout)
	cmd.Stderr = orDiscard(spec.Stderr)

	// Bound the time I/O pipes may stay open after process exit or
	// context cancellation, so Wait can never hang on a stuck pipe.
	cmd.WaitDelay = pipeDrainDelay

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, launchError(spec, err)
	}

	job, err := newJob()
	if err != nil {
		// Cannot happen on Unix (newJob never fails), but keep the
		// contract: never leak a started child.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		return nil, err
	}

	if bindErr := job.assign(cmd.Process.Pid); bindErr != nil {
		job.close()

		// Supervised fallback: the process group still provides
		// deterministic cleanup; the degradation is recorded and
		// surfaced, never silent.
		return supervise(ctx, spec, cmd, nil, false, bindErr.Error(), cancelLaunch)
	}

	return supervise(ctx, spec, cmd, job, true, "", cancelLaunch)
}

// reapDescendants enforces the no-descendant-survives invariant after
// the direct child has exited (any exit path). SIGKILL to the group
// is idempotent — an empty or fully-dead group returns ESRCH, which
// is the success case — and covers every member at once, including
// grandchildren that ignored the polite SIGTERM.
func reapDescendants(m *ManagedProcess) {
	if m == nil || m.pid <= 0 {
		return
	}

	_ = syscall.Kill(-m.pid, syscall.SIGKILL)
}

// politeShutdown sends SIGTERM to the whole process group (the group
// leader is covered by the negative-pid form). Children that ignore
// SIGTERM are handled by the hard-kill phase.
func politeShutdown(m *ManagedProcess) {
	if m == nil || m.pid <= 0 {
		return
	}

	_ = syscall.Kill(-m.pid, syscall.SIGTERM)
}

// hardKill SIGKILLs the entire process group: the child, every
// descendant, in one kernel operation. Also signals the leader
// directly in case it left its own group.
func hardKill(m *ManagedProcess) {
	if m == nil || m.pid <= 0 {
		return
	}

	_ = syscall.Kill(-m.pid, syscall.SIGKILL)
	_ = syscall.Kill(m.pid, syscall.SIGKILL)

	logging.D("system", "hard_kill", "process group %d killed", m.pid)
}
