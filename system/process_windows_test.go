//go:build windows

package system

import (
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Windows test fixtures. The system shell is resolved through the
// launcher's own resolver (COMSPEC / System32), so the tests never
// depend on the working directory or the inherited PATH — the exact
// property the v0.8.0 contract requires.

// streamStdout / streamStderr select the echo target.
const (
	streamStdout = iota
	streamStderr
)

// exitSpec builds a spec that exits with the given code.
func exitSpec(t *testing.T, code int) ProcessSpec {
	t.Helper()

	return ProcessSpec{
		Name: "test-exit",
		Path: "cmd.exe",
		Args: []string{"/c", "exit", strconv.Itoa(code)},
	}
}

// echoSpec builds a spec that writes marker to the chosen stream.
func echoSpec(t *testing.T, marker string, stream int) ProcessSpec {
	t.Helper()

	if stream == streamStderr {
		return ProcessSpec{
			Name: "test-echo-stderr",
			Path: "cmd.exe",
			Args: []string{"/c", "echo", marker, "1>&2"},
		}
	}

	return ProcessSpec{
		Name: "test-echo-stdout",
		Path: "cmd.exe",
		Args: []string{"/c", "echo", marker},
	}
}

// longRunSpec builds a spec that stays alive for ~60s until killed.
func longRunSpec(t *testing.T) ProcessSpec {
	t.Helper()

	return ProcessSpec{
		Name: "test-longrun",
		Path: "cmd.exe",
		Args: []string{"/c", "ping", "-n", "60", "127.0.0.1"},
	}
}

// grandchildSpec builds a spec whose child spawns a long-lived
// grandchild (start /b) and then stays alive itself: both must die
// with the supervisor. Arguments are passed as separate argv entries
// so exec's Windows quoting never mangles the command separators.
func grandchildSpec(t *testing.T) ProcessSpec {
	t.Helper()

	return ProcessSpec{
		Name: "test-grandchild",
		Path: "cmd.exe",
		Args: []string{"/c", "start", "/b", "ping", "-n", "60", "127.0.0.1",
			"&", "ping", "-n", "60", "127.0.0.1"},
	}
}

// waitForGrandchild blocks until the child has at least one living
// descendant, observed through the kernel (job member list when
// bound, Toolhelp32 tree otherwise), then returns the observed
// descendant pids. Deterministic: no fixed sleeps, bounded polling.
func waitForGrandchild(t *testing.T, m *ManagedProcess) []int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		var ids []int

		m.mu.Lock()
		job := m.job
		bound := m.jobBound
		m.mu.Unlock()

		if bound && job != nil {
			if members, ok := job.processIDs(); ok {
				ids = members
			}
		} else {
			ids = processTreeIDs(m.pid)
		}

		var descendants []int

		for _, id := range ids {
			if id != m.pid {
				descendants = append(descendants, id)
			}
		}

		if len(descendants) > 0 {
			return descendants
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("grandchild never observed: child did not spawn a descendant in 5s")

	return nil
}

// assertNoSupervisedProcessSurvives proves the no-orphan invariant on
// Windows: the direct child is dead AND every process in its
// supervision domain (job members or Toolhelp32 descendants) is dead,
// including any known descendants observed before the stop.
func assertNoSupervisedProcessSurvives(t *testing.T, m *ManagedProcess, known ...int) {
	t.Helper()

	if m.Running() {
		t.Fatal("supervised process still running")
	}

	for _, id := range append(known, m.pid) {
		if childAlive(id) {
			t.Fatalf("pid %d survived supervisor shutdown", id)
		}
	}

	// Belt and braces: snapshot-walk the tree of the (now dead) child.
	for _, id := range processTreeIDs(m.pid) {
		if childAlive(id) {
			t.Fatalf("tree member pid %d survived supervisor shutdown", id)
		}
	}
}

// assertChildNotAttachedToParentConsole verifies the behavioural half
// of the no-visible-console guarantee: a child created with
// CREATE_NO_WINDOW never attaches to the parent's console, so it must
// be absent from the parent's console process list. When the test
// binary itself runs without a console (redirected output — common on
// runners) the enumeration is unavailable and the check is skipped
// with a log entry; the flag itself remains asserted by source
// construction in process_windows.go.
func assertChildNotAttachedToParentConsole(t *testing.T, pid int) {
	t.Helper()

	list := make([]uint32, 128)

	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(len(list)), uintptr(unsafe.Pointer(&list[0])))
	if n == 0 {
		t.Log("test process has no console; console-attach assertion skipped")

		return
	}

	count := int(n)
	if count > len(list) {
		count = len(list)
	}

	for i := 0; i < count; i++ {
		if int(list[i]) == pid {
			t.Fatalf("pid %d attached to the parent console; "+
				"CREATE_NO_WINDOW was not applied", pid)
		}
	}
}

// restrictedBindErrno is the exact restricted-environment signature:
// ERROR_ACCESS_DENIED from AssignProcessToJobObject.
func restrictedBindErrno() error {
	return syscall.Errno(errorAccessDenied)
}

var procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")

// ---- RunProbe platform fixtures (windows) ---------------------------------
//
// Compound probe commands for the synchronous helper's tests; the
// unix counterparts live in process_unix_test.go with identical
// signatures. Arguments are passed as one command line after /c so
// cmd's separators (&, 1>&2) are not mangled by exec quoting.

// probeEchoBothSpec writes marker-stdout to stdout and marker-stderr
// to stderr, then exits 0.
func probeEchoBothSpec(t *testing.T, marker string) ProcessSpec {
	t.Helper()

	return ProcessSpec{
		Name: "probe-echo-both",
		Path: "cmd.exe",
		Args: []string{"/c", "echo " + marker + "-stdout& echo " + marker + "-stderr 1>&2"},
	}
}

// probeFailSpec writes marker, then exits 7.
func probeFailSpec(t *testing.T, marker string) ProcessSpec {
	t.Helper()

	return ProcessSpec{
		Name: "probe-fail",
		Path: "cmd.exe",
		Args: []string{"/c", "echo " + marker + "& exit 7"},
	}
}

// probeBigOutputSpec emits more than the probe output cap (64 KiB):
// 8000 lines of ~72 bytes ≈ 576 KiB.
func probeBigOutputSpec(t *testing.T) ProcessSpec {
	t.Helper()

	return ProcessSpec{
		Name: "probe-big",
		Path: "cmd.exe",
		Args: []string{"/c", "for /L %i in (1,1,8000) do @echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
}
