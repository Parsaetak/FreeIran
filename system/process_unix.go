//go:build !windows

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
	return name
}

func platformNote() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// launchProcess starts a managed process with stdout/stderr captured.
// The child runs in its own process group so a forced kill cannot
// leak grandchildren.
//
// Output capture: when ProcessSpec carries Stdout/Stderr writers the
// child's streams are connected to them (exec creates the pipes and
// copies internally); otherwise output goes to io.Discard. cmd.Wait
// waits for the copy goroutines, so the writers are complete once the
// exited channel closes.
func launchProcess(ctx context.Context, spec ProcessSpec) (*ManagedProcess, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.WorkDir

	cmd.Stdout = orDiscard(spec.Stdout)
	cmd.Stderr = orDiscard(spec.Stderr)

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindDependencyUnavailable,
			Subsystem, "start", "launch %q", spec.Path)
	}

	proc := &ManagedProcess{
		spec:      spec,
		StartedAt: time.Now().UTC(),
		exited:    make(chan struct{}),
		pid:       cmd.Process.Pid,
	}

	go func() {
		proc.err = cmd.Wait()

		// A polite termination racing a normal exit is not an error.
		if proc.err != nil && proc.wasStopped() {
			proc.err = nil
		}

		close(proc.exited)
	}()

	return proc, nil
}

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

// terminateProcess stops the process politely, then forcibly.
func terminateProcess(m *ManagedProcess, grace time.Duration) error {
	process, err := os.FindProcess(m.pid)
	if err != nil {
		return nil // already gone
	}

	_ = process.Signal(syscall.SIGTERM)

	select {
	case <-m.exited:
		return nil

	case <-time.After(grace):
		_ = process.Signal(syscall.SIGKILL)
		<-m.exited

		return nil
	}
}
