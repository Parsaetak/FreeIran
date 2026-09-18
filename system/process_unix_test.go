//go:build !windows

package system

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Unix test fixtures. Everything runs through /bin/sh (one portable
// dependency) with process-group supervision semantics.

const (
	streamStdout = iota
	streamStderr
)

// shSpec builds a spec running one shell command.
func shSpec(name, command string) ProcessSpec {
	return ProcessSpec{
		Name: name,
		Path: "/bin/sh",
		Args: []string{"-c", command},
	}
}

func exitSpec(t *testing.T, code int) ProcessSpec {
	t.Helper()

	if !lookPathAvailable("/bin/sh") {
		t.Skip("/bin/sh not available")
	}

	return shSpec("test-exit", fmt.Sprintf("exit %d", code))
}

func echoSpec(t *testing.T, marker string, stream int) ProcessSpec {
	t.Helper()

	if !lookPathAvailable("/bin/sh") {
		t.Skip("/bin/sh not available")
	}

	if stream == streamStderr {
		return shSpec("test-echo-stderr", "echo "+marker+" >&2")
	}

	return shSpec("test-echo-stdout", "echo "+marker)
}

func longRunSpec(t *testing.T) ProcessSpec {
	t.Helper()

	if !lookPathAvailable("/bin/sh") {
		t.Skip("/bin/sh not available")
	}

	return shSpec("test-longrun", "sleep 60")
}

// grandchildSpec: the child spawns a long-lived grandchild (sleep),
// announces its pid to a file (deterministic observation — no fixed
// sleeps), then waits. Both must die with the supervisor.
func grandchildSpec(t *testing.T) ProcessSpec {
	t.Helper()

	if !lookPathAvailable("/bin/sh") {
		t.Skip("/bin/sh not available")
	}

	pidfile := grandchildPidFile()

	// No stale announcement from an earlier run, and no leftover
	// after the test finishes.
	_ = os.Remove(pidfile)
	t.Cleanup(func() { _ = os.Remove(pidfile) })

	// The temp path contains no spaces, so embedding it directly in
	// the command is safe.
	return shSpec("test-grandchild", "sleep 60 & echo $! > "+pidfile+"; wait")
}

// grandchildPidFile returns the deterministic announcement path
// shared by the writer (the child shell) and the reader (the test).
// Keyed by the test-binary pid: tests run serially, so there is
// exactly one live announcement at a time.
func grandchildPidFile() string {
	return filepath.Join(os.TempDir(),
		fmt.Sprintf("freeiran-grandchild-%d.pid", os.Getpid()))
}

// waitForGrandchild reads the announced grandchild pid — the file
// write IS the deterministic completion signal — with bounded
// polling, and returns it.
func waitForGrandchild(t *testing.T, m *ManagedProcess) []int {
	t.Helper()

	pidfile := grandchildPidFile()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidfile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return []int{pid}
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("grandchild pid never announced")

	return nil
}

// assertNoSupervisedProcessSurvives proves the no-orphan invariant on
// Unix: the direct child is dead, every known descendant is dead
// (zombie-tolerant — a zombie holds no files, locks or execution),
// and the process group has no live members left. Signal delivery is
// asynchronous, so the assertion converges on a bounded deadline —
// the reap (group SIGKILL) was already issued before exit observers
// woke, so death is certain; the poll only waits for the kernel to
// reflect it.
func assertNoSupervisedProcessSurvives(t *testing.T, m *ManagedProcess, known ...int) {
	t.Helper()

	if m.Running() {
		t.Fatal("supervised process still running")
	}

	deadline := time.Now().Add(5 * time.Second)

	for {
		problems := 0

		// Direct child: reaped by cmd.Wait, so a signal-0 probe must
		// report it gone.
		if err := syscall.Kill(m.pid, 0); err != syscall.ESRCH {
			problems++
		}

		for _, id := range known {
			if processAliveZombieTolerant(id) {
				problems++
			}
		}

		if groupHasLiveMembers(m.pid) {
			problems++
		}

		if problems == 0 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("supervision domain of pid %d still has live members "+
				"(known descendants %v)", m.pid, known)
		}

		time.Sleep(25 * time.Millisecond)
	}
}

// assertChildNotAttachedToParentConsole is a Windows-only assertion;
// on Unix there is no console window concept for supervised cores.
func assertChildNotAttachedToParentConsole(t *testing.T, pid int) {
	t.Helper()
}

// restrictedBindErrno mirrors the Windows restricted-environment
// injection with a plain error (Unix has no job hierarchy, so the
// retry tier is not reachable — the fallback lifecycle still runs).
func restrictedBindErrno() error {
	return fmt.Errorf("injected ERROR_ACCESS_DENIED-equivalent (unix)")
}

// processAliveZombieTolerant reports whether pid still executes. A
// zombie (state Z) or dead state (X) counts as dead: zombies hold no
// open files or locks and cannot run. /proc is authoritative on
// Linux; elsewhere the signal-0 probe is used.
func processAliveZombieTolerant(pid int) bool {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		text := string(data)

		if idx := strings.LastIndex(text, ")"); idx >= 0 && idx+2 <= len(text) {
			fields := strings.Fields(text[idx+2:])
			if len(fields) > 0 {
				state := fields[0]

				return state != "Z" && state != "X"
			}
		}

		return true
	}

	return syscall.Kill(pid, 0) == nil
}

// groupHasLiveMembers reports whether the process group pgid still
// has at least one executing member (zombies excluded on Linux).
func groupHasLiveMembers(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// No /proc (non-Linux unix): fall back to the signal probe —
		// init reaps orphans promptly on those platforms.
		return syscall.Kill(-pgid, 0) != syscall.ESRCH
	}

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}

		text := string(data)
		idx := strings.LastIndex(text, ")")
		if idx < 0 || idx+2 > len(text) {
			continue
		}

		fields := strings.Fields(text[idx+2:])
		if len(fields) < 3 {
			continue
		}

		state := fields[0]
		pgrp, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}

		if pgrp == pgid && state != "Z" && state != "X" {
			return true
		}
	}

	return false
}

// ---- RunProbe platform fixtures (unix) -----------------------------------
//
// Compound probe commands for the synchronous helper's tests; the
// Windows counterparts live in process_windows_test.go with identical
// signatures.

// probeEchoBothSpec writes marker-stdout to stdout and marker-stderr
// to stderr, then exits 0.
func probeEchoBothSpec(t *testing.T, marker string) ProcessSpec {
	t.Helper()

	return shSpec("probe-echo-both", "echo "+marker+"-stdout; echo "+marker+"-stderr >&2")
}

// probeFailSpec writes marker, then exits 7.
func probeFailSpec(t *testing.T, marker string) ProcessSpec {
	t.Helper()

	return shSpec("probe-fail", "echo "+marker+"; exit 7")
}

// probeBigOutputSpec emits ~4x the probe output cap.
func probeBigOutputSpec(t *testing.T) ProcessSpec {
	t.Helper()

	return shSpec("probe-big", "head -c 262144 /dev/zero | tr '\\0' 'x'")
}
