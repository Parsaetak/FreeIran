//go:build !windows

package system

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
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
func launchProcess(ctx context.Context, spec ProcessSpec) (*ManagedProcess, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.WorkDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "start", "stdout pipe")
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "start", "stderr pipe")
	}

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

	// Drain pipes so the child never blocks on full buffers. The
	// captured output is discarded here; callers needing logs should
	// pass a logging wrapper through ProcessSpec.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

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

// queryCoreVersion asks a core for its version string.
func queryCoreVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return ""
	}

	return strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
}
