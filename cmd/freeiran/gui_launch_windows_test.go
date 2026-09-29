//go:build windows

package main

// v0.11.5 — the REAL Windows GUI launch proof (P0-B of the release).
//
// The pre-existing checks are necessary but insufficient, which is how
// the v0.11.4 "process boots, no window ever appears" defect shipped
// with a green CI:
//
//   - TestWindowsGUISubsystem (gui_check_windows_test.go) proves the PE
//     header declares the WINDOWS_GUI subsystem — a binary property;
//   - the --smoke-test runtime smoke boots the ENGINE but exits before
//     the Wails webview is ever constructed.
//
// Neither one launches the actual desktop application. This test does:
// it starts the built FreeIran.exe WITHOUT --smoke-test, in an isolated
// workspace (FREEIRAN_HOME → temp dir, so the user/CI machine's real
// data and the tray close-to-tray setting are untouched), and then
// polls the native window station for a VISIBLE top-level window owned
// by the launched process (user32 EnumWindows + IsWindowVisible +
// GetWindowThreadProcessId + GetWindowTextW). "Process alive" and "GUI
// visible" are distinct observations and both are reported.
//
// Failure diagnostics: the exact process state (exited vs running), the
// full window inventory owned by the child PID (visible and hidden),
// the child's stdout/stderr capture and the tail of the isolated
// runtime log — instead of "waited longer".
//
// Termination: the child is killed by its exact PID (taskkill /T /F, no
// image-name matching) on every exit path. The forceful kill is what
// prevents a default tray_enabled=true close-to-tray setting from ever
// stranding the test in the tray; the isolated FREEIRAN_HOME keeps the
// host's real settings out of the picture. The existing PE-subsystem
// test is preserved unchanged in gui_check_windows_test.go.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// user32 bindings (smallest reliable equivalent of a native HWND
// observation — no cgo, no third-party dependency).
var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
)

// visibleWindowsOwnedBy returns the titles of every VISIBLE top-level
// window owned by pid, plus the count of all top-level windows (visible
// or not) owned by pid — the "process alive but window hidden" probe.
func visibleWindowsOwnedBy(pid uint32) (visible []string, owned int) {
	cb := syscall.NewCallback(func(hwnd windows.HWND, lparam uintptr) uintptr {
		var owner uint32

		procGetWindowThreadProcessID.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&owner)))

		if owner != pid {
			return 1 // continue enumeration
		}

		owned++

		visibleVal, _, _ := procIsWindowVisible.Call(uintptr(hwnd))

		title := windowTitle(hwnd)

		if visibleVal != 0 && title != "" {
			visible = append(visible, title)
		}

		return 1 // continue enumeration
	})

	procEnumWindows.Call(cb, 0)

	return visible, owned
}

func windowTitle(hwnd windows.HWND) string {
	const bufSize = 256

	buf := make([]uint16, bufSize)

	n, _, _ := procGetWindowTextW.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(bufSize),
	)

	return windows.UTF16ToString(buf[:n])
}

// TestWindowsGUILaunchProof launches the built FreeIran.exe (the REAL
// desktop application, no --smoke-test) and proves a visible top-level
// window owned by that process appears within a bounded timeout.
//
// FREEIRAN_GUI_EXE must name the executable (the CI/release workflows
// and `make desktop-windows` set it after linking; without it the test
// skips so plain `go test ./...` runs are unaffected).
func TestWindowsGUILaunchProof(t *testing.T) {
	exe := os.Getenv("FREEIRAN_GUI_EXE")
	if exe == "" {
		t.Skip("FREEIRAN_GUI_EXE not set; skipping the GUI launch proof " +
			"(the CI/release workflows set it to the built FreeIran.exe)")
	}

	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("FREEIRAN_GUI_EXE %s: %v", exe, err)
	}

	// Isolated workspace: the child writes logs, settings and store
	// data HERE, never into the real user profile. A fresh directory
	// also means deterministic defaults (tray_enabled default true) —
	// and the force-kill termination below is what keeps that tray
	// from stranding the instance.
	home := t.TempDir()

	cmd := exec.Command(exe) // NO --smoke-test: the GUI path is the subject
	cmd.Env = append(os.Environ(), "FREEIRAN_HOME="+home)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: false}

	if err := cmd.Start(); err != nil {
		t.Fatalf("launch %s: %v", exe, err)
	}

	pid := uint32(cmd.Process.Pid)

	exited := make(chan error, 1)

	go func() { exited <- cmd.Wait() }()

	t.Cleanup(func() {
		// Exact-PID force-kill (tree): no image-name matching, no tray
		// stranding, no interference with any other FreeIron instance.
		kill := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(pid))
		kill.Run() //nolint:errcheck — best effort; the Wait below is authoritative

		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
	})

	// Bounded GUI-visibility window. Generous enough for a cold CI
	// runner (engine boot + WebView2 environment creation), short
	// enough to fail loudly instead of hanging the job.
	const (
		pollInterval = 250 * time.Millisecond
		guiDeadline  = 45 * time.Second
	)

	deadline := time.Now().Add(guiDeadline)
	started := time.Now()

	var lastOwned int

	for {
		select {
		case err := <-exited:
			// The process DIED before any window could be proven:
			// dump everything observable, then fail with the exit
			// status — this is the v0.11.4 defect signature.
			dumpLaunchDiagnostics(t, home, &stdout, &stderr)
			t.Fatalf("process exited before a visible window appeared: %v", err)

		default:
		}

		visible, owned := visibleWindowsOwnedBy(pid)
		lastOwned = owned

		if len(visible) > 0 {
			t.Logf("GUI visible after %s: %d visible top-level window(s) owned by PID %d: %q",
				time.Since(started).Round(time.Millisecond),
				len(visible), pid, visible)

			// The visible-window proof is complete. Terminate the
			// isolated instance (t.Cleanup) and succeed.
			return
		}

		if time.Now().After(deadline) {
			dumpLaunchDiagnostics(t, home, &stdout, &stderr)
			t.Fatalf("no visible top-level window owned by PID %d within %s "+
				"(process still alive; owned top-level windows, visible or hidden: %d) — "+
				"the GUI became visible, or the process would have exited, in a healthy launch",
				pid, guiDeadline, lastOwned)
		}

		time.Sleep(pollInterval)
	}
}

// dumpLaunchDiagnostics prints the failure-time evidence: child output
// capture and the tail of the isolated runtime log (the same log the
// wails log bridge writes WebView2 failures into).
func dumpLaunchDiagnostics(t *testing.T, home string, stdout, stderr *bytes.Buffer) {
	t.Helper()

	if out := strings.TrimSpace(stdout.String()); out != "" {
		t.Logf("child stdout (tail):\n%s", tailLines(out, 20))
	}

	if errOut := strings.TrimSpace(stderr.String()); errOut != "" {
		t.Logf("child stderr (tail):\n%s", tailLines(errOut, 20))
	}

	logPath := filepath.Join(home, "logs", "freeiran.log")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Logf("isolated runtime log %s unreadable: %v", logPath, err)
		return
	}

	text := strings.ReplaceAll(string(raw), "\r\n", "\n")

	t.Logf("isolated runtime log %s (tail):\n%s", logPath, tailLines(text, 40))
}

func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, "\n")
}
