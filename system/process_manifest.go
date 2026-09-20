// process_manifest.go maintains the managed-process manifest: the
// on-disk record of every process FreeIran currently owns.
//
// WHY THIS EXISTS: kernel job objects (KILL_ON_JOB_CLOSE) already
// guarantee that supervised descendants die with the application —
// the manifest is NOT the cleanup mechanism for a normal shutdown.
// It is the OWNERSHIP EVIDENCE for external, path-verified cleanup
// (the Windows installer): a process may be terminated as "owned"
// only when its PID is recorded here AND its current executable path
// matches the recorded path. Bare image-name kills (taskkill /im
// xray.exe) can hit an unrelated user process with the same name and
// are therefore forbidden in the installer; the manifest lets it kill
// exactly the processes FreeIran spawned and nothing else.
//
// Format: one line per process, "pid|absolute-executable-path". The
// file lives in the workspace runtime directory (short-lived state,
// always cleaned) and is rewritten on every membership change. A
// crash mid-write leaves at most one partial line, which readers
// skip. Stale entries (PID reuse after a crash) are inert: the
// path-verification step refuses to kill a reused PID whose image no
// longer matches.
package system

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// processManifestPath is the manifest file path ("" = recording
// disabled — library use and tests). Set once by the composition
// root before any managed process starts.
var processManifestPath string

// manifestMu guards the manifest registry and its file.
var manifestMu sync.Mutex

// manifestPIDs is the in-memory registry: pid → executable path.
var manifestPIDs = map[int]string{}

// SetProcessManifestPath configures where the managed-process
// manifest is recorded. Passing an empty string disables recording
// (the default). The composition root calls this once during boot
// with the workspace runtime directory, before any core or provider
// process starts.
func SetProcessManifestPath(path string) {
	manifestMu.Lock()
	defer manifestMu.Unlock()

	processManifestPath = path
}

// ProcessManifestPath returns the configured manifest path ("" when
// recording is disabled).
func ProcessManifestPath() string {
	manifestMu.Lock()
	defer manifestMu.Unlock()

	return processManifestPath
}

// manifestRecord adds a process to the manifest. Called from
// supervise() after a successful spawn.
func manifestRecord(pid int, execPath string) {
	if pid <= 0 || execPath == "" {
		return
	}

	manifestMu.Lock()
	defer manifestMu.Unlock()

	if processManifestPath == "" {
		return
	}

	if abs, err := filepath.Abs(execPath); err == nil {
		execPath = abs
	}

	manifestPIDs[pid] = execPath
	manifestWriteLocked()
}

// manifestRemove drops a process from the manifest. Called from
// finish() once the process has terminated; the empty registry
// removes the file entirely so a cleanly stopped application leaves
// no stale manifest behind.
func manifestRemove(pid int) {
	manifestMu.Lock()
	defer manifestMu.Unlock()

	if processManifestPath == "" {
		return
	}

	if _, ok := manifestPIDs[pid]; !ok {
		return
	}

	delete(manifestPIDs, pid)
	manifestWriteLocked()
}

// manifestWriteLocked rewrites the manifest file from the in-memory
// registry (caller holds manifestMu). Failures are best-effort: a
// missing manifest only degrades external cleanup to the kernel
// job-object guarantee, which is the primary mechanism anyway.
func manifestWriteLocked() {
	if len(manifestPIDs) == 0 {
		_ = os.Remove(processManifestPath)
		return
	}

	var sb strings.Builder

	for pid, path := range manifestPIDs {
		// One line per process; pid and path are separated by '|',
		// which cannot appear in a Windows path.
		sb.WriteString(strconv.Itoa(pid))
		sb.WriteByte('|')
		sb.WriteString(path)
		sb.WriteByte('\n')
	}

	_ = os.WriteFile(processManifestPath, []byte(sb.String()), 0o600)
}

// ReadProcessManifest parses a manifest file into its pid → path
// entries, skipping blank or malformed lines. Exposed for tests and
// diagnostics.
func ReadProcessManifest(path string) map[int]string {
	out := map[int]string{}

	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		pidText, path, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}

		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 || path == "" {
			continue
		}

		out[pid] = path
	}

	return out
}

// ManifestEntries returns a copy of the live registry (diagnostics).
func ManifestEntries() map[int]string {
	manifestMu.Lock()
	defer manifestMu.Unlock()

	out := make(map[int]string, len(manifestPIDs))
	for pid, path := range manifestPIDs {
		out[pid] = path
	}

	return out
}

// manifestLine renders one entry (test helper).
func manifestLine(pid int, path string) string {
	return fmt.Sprintf("%d|%s\n", pid, path)
}
