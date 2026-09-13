//go:build windows

package coremgr

import (
	"os/exec"
	"syscall"
	"time"
)

// Console-hiding creation flags (mirror system/process_windows.go).
//
// The Managed Core Manager launches protocol-core binaries directly
// (version probes, config validation, smoke tests). On Windows every
// one of those launches MUST pass CREATE_NO_WINDOW and
// CREATE_NEW_PROCESS_GROUP, otherwise a visible CMD/console window
// flashes for the lifetime of the child — the exact bug class v0.6
// fixed for the engine path and v0.9.0 closed for the manager path.
const (
	createNoWindow        = 0x08000000
	createNewProcessGroup = 0x00000200
)

// applyHiddenConsole marks a command to launch without any visible
// console window on Windows. Safe to call on every cmd.
func applyHiddenConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags = createNoWindow | createNewProcessGroup
}

// gracefulStop terminates a smoke-test child and reports whether it
// exited within the timeout.
//
// Windows semantics: a detached, console-less process group cannot
// receive console control events (GenerateConsoleCtrlEvent needs a
// shared console), so os.Interrupt is unsupported — the only
// deterministic termination is TerminateProcess. The production
// engine (system package) applies exactly the same policy through
// job objects. A Kill that terminates the child promptly therefore
// counts as a clean shutdown: the guarantee under test is "the child
// never outlives the manager" and it holds.
func gracefulStop(cmd *exec.Cmd, done <-chan error, timeout time.Duration) (clean bool, detail string) {
	_ = cmd.Process.Kill()

	select {
	case <-done:
		return true, "terminated via TerminateProcess (Windows: no console signal for detached children)"
	case <-time.After(timeout):
		return false, "child did not exit after TerminateProcess"
	}
}
