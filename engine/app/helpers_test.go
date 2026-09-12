package app

import (
	"os"
	"path/filepath"
)

// writeFile is a test helper for creating legacy fixture files.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// listOpenFilesUnder reports any file descriptor in /proc/self/fd
// pointing at a path under root (Linux only). Used by the lifecycle
// tests to prove no handle outlives Shutdown — the Windows-critical
// guarantee observed directly on Linux.
func listOpenFilesUnder(root string) []string {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}

	var leaks []string

	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			continue
		}

		if rel, err := filepath.Rel(abs, target); err == nil &&
			rel != ".." && rel != "." && len(rel) < len(target) {
			leaks = append(leaks, target)
		}
	}

	return leaks
}
