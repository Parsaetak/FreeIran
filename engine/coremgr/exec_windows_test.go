//go:build windows

package coremgr

import (
	"os/exec"
	"testing"
)

// TestApplyHiddenConsole verifies that every manager-spawned child is
// marked CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP and HideWindow —
// the regression guard for the "CMD window stays open" bug class. The
// Windows CI job runs this on every push; the flag constants must
// never regress to zero.
func TestApplyHiddenConsole(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo", "hi") //nolint:gosec // test fixture
	applyHiddenConsole(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr not set by applyHiddenConsole")
	}

	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow = false, want true")
	}

	want := uint32(createNoWindow | createNewProcessGroup)
	if got := cmd.SysProcAttr.CreationFlags; got&want != want {
		t.Errorf("CreationFlags = %#x, want at least %#x", got, want)
	}

	// Idempotent: applying twice must not corrupt the attributes.
	applyHiddenConsole(cmd)
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow lost after second applyHiddenConsole")
	}
}

// TestApplyHiddenConsoleNilSafe ensures a nil cmd never panics.
func TestApplyHiddenConsoleNilSafe(t *testing.T) {
	applyHiddenConsole(nil) // must not panic
}

// TestGracefulStopTerminatesChild verifies gracefulStop deterministically
// terminates a real child process (cmd.exe echo) and reports clean.
func TestGracefulStopTerminatesChild(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "ping -n 30 127.0.0.1 >nul") //nolint:gosec // test fixture
	applyHiddenConsole(cmd)

	if err := cmd.Start(); err != nil {
		t.Skipf("cmd.exe unavailable: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	clean, _ := gracefulStop(cmd, done, 5e9)

	if !clean {
		t.Error("gracefulStop reported unclean shutdown for a killable child")
	}

	if cmd.ProcessState == nil {
		t.Error("child was not reaped")
	}
}
