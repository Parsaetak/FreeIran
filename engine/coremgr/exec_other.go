//go:build !windows

package coremgr

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// applyHiddenConsole is a no-op on non-Windows platforms: Unix has no
// console-creation semantics. It exists so every launch site in the
// manager can call it unconditionally.
func applyHiddenConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	// Own the process group so the child never receives terminal
	// signals aimed at the parent test/app process.
	cmd.SysProcAttr.Setpgid = true
}

// gracefulStop terminates a smoke-test child politely and reports
// whether it exited within the timeout. Unix cores receive SIGINT
// (their documented graceful-stop signal) before a hard SIGKILL.
func gracefulStop(cmd *exec.Cmd, done <-chan error, timeout time.Duration) (clean bool, detail string) {
	if err := cmd.Process.Signal(os.Interrupt); err == nil {
		select {
		case <-done:
			return true, "exited after SIGINT"
		case <-time.After(timeout):
		}
	}

	_ = cmd.Process.Kill()

	select {
	case <-done:
		return false, "exited after SIGKILL (ignored SIGINT)"
	case <-time.After(timeout):
		return false, "child did not exit after SIGKILL"
	}
}
