//go:build windows

package system

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

func execLookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func executableName(name string) string {
	return name + ".exe"
}

func platformNote() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// launchProcess starts a managed process with stdout/stderr captured
// WITHOUT opening a visible CMD/console window. Windows-specific flags:
//
//   - CREATE_NO_WINDOW (0x08000000): the child has no console at all.
//     This is the flag the upstream Xray/v2rayN/sing-box launchers use
//     to keep the user desktop clean when a protocol core is spawned.
//   - CREATE_NEW_PROCESS_GROUP (0x00000200): the child is the root of
//     a new process group so a Ctrl-Break / group-wide signal does not
//     bleed into sibling FreeIran subsystems.
//   - DETACHED_PROCESS (0x00000008): additional belt-and-braces so a
//     forked console child cannot attach to FreeIran's window station.
//
// Output capture: when ProcessSpec carries Stdout/Stderr writers the
// child's streams are connected to them through exec's own pipes; the
// pipes are created without inheriting any console handle. cmd.Wait
// waits for the copy goroutines, so the writers are complete once the
// exited channel closes.
//
// Job-object binding: a Windows job object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE guarantees that no protocol-core
// process can outlive FreeIran. If FreeIran crashes, is killed from
// Task Manager, or terminates abnormally, the OS kernel reaps every
// child the job holds — the "no orphan core remains after exit"
// guarantee is enforced by the kernel, not by graceful shutdown code.
func launchProcess(ctx context.Context, spec ProcessSpec) (*ManagedProcess, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.WorkDir

	cmd.Stdout = orDiscard(spec.Stdout)
	cmd.Stderr = orDiscard(spec.Stderr)

	// CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS.
	// HidePin makes stderr/stdout pipes still work because exec
	// allocates non-inherited anonymous pipes for them rather than
	// reusing the parent's console handles.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow | createNewProcessGroup | detachedProcess,
	}

	if err := cmd.Start(); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindDependencyUnavailable,
			Subsystem, "start", "launch %q", spec.Path)
	}

	// Bind the child to a kill-on-close job object so an abnormal
	// FreeIran exit reaps the protocol core. The job handle is owned
	// by the ManagedProcess and closed in terminateProcess.
	job, err := newKillOnCloseJob(cmd.Process.Pid)
	if err != nil {
		// Job binding failure is not fatal: terminate the child we
		// just spawned so we never leak a process, then surface the
		// underlying launch error.
		_ = cmd.Process.Kill()
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "start", "bind kill-on-close job")
	}

	proc := &ManagedProcess{
		spec:      spec,
		StartedAt: time.Now().UTC(),
		exited:    make(chan struct{}),
		pid:       cmd.Process.Pid,
		job:       job,
	}

	go func() {
		proc.err = cmd.Wait()

		if proc.err != nil && proc.wasStopped() {
			proc.err = nil
		}

		close(proc.exited)
	}()

	return proc, nil
}

// Windows process creation flags. Defined here as constants so the
// intent of every flag is documented at the point of use.
const (
	createNoWindow        = 0x08000000
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// orDiscard defaults absent writers to the sink so the child never
// blocks on full pipe buffers.
func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}

	return w
}

func (m *ManagedProcess) wasStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.stopped
}

// terminateProcess stops a Windows process. Windows has no SIGTERM:
// Kill is the only portable mechanism, so the grace period becomes a
// last-resort timeout. The kill-on-close job handle is released after
// the process has exited so the kernel no longer tracks it.
func terminateProcess(m *ManagedProcess, grace time.Duration) error {
	process, err := os.FindProcess(m.pid)
	if err != nil {
		m.closeJob()
		return nil // already gone
	}

	select {
	case <-m.exited:
		m.closeJob()
		return nil

	case <-time.After(grace):
		_ = process.Kill()
		<-m.exited
		m.closeJob()

		return nil
	}
}
