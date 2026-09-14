// runtime_root.go pins the directory under which temporary per-launch
// runtime-config directories are created (v0.9.2 workspace model).
//
// Before v0.9.2 core run configs landed under the system temp root
// (%TEMP% / XDG tmp): runtime state was silently split across roots
// and leftover directories were invisible to cleanup. The composition
// root (engine/app) now points this at <workspace>/runtime, so:
//
//   - every runtime artifact lives below the single workspace root;
//   - the runtime cleanup task can scan ONE known directory with a
//     cheap prefix filter instead of the whole system temp tree;
//   - nothing changes for tests, which pass explicit WorkDirs.
//
// Default (never Set): the system temp root — the historical behavior
// — so library users without a workspace keep working.
package core

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// runtimeRoot holds the configured runtime directory ("") = system temp.
var runtimeRoot atomic.Value // string

// SetRuntimeRoot configures the directory under which temporary
// per-launch runtime-config directories are created. Passing an empty
// string restores the system-temp default. The composition root calls
// this once during boot, before any core launch.
func SetRuntimeRoot(dir string) {
	runtimeRoot.Store(dir)
}

// RuntimeRoot returns the current runtime directory root.
func RuntimeRoot() string {
	if value, ok := runtimeRoot.Load().(string); ok && value != "" {
		return value
	}

	return os.TempDir()
}

// RemoveStaleRunConfigs deletes leftover per-launch runtime-config
// directories older than maxAge. Run configs are reconstructable
// state (the owning core instance removes its own directory on Stop;
// leftovers only exist after a hard kill or crash). The scan is
// bounded to the configured runtime root with the freeiran- prefix
// and a maxEntries cap. Returns the bytes reclaimed.
func RemoveStaleRunConfigs(ctx context.Context, maxAge time.Duration, maxEntries int) (int64, error) {
	if maxAge <= 0 {
		maxAge = 6 * time.Hour
	}

	if maxEntries <= 0 {
		maxEntries = 256
	}

	root := runtimeRootValue()
	if root == "" {
		// Historical default: scan the system temp root for our prefix.
		root = os.TempDir()
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, nil // root missing: nothing to clean
	}

	var reclaimed int64

	removed := 0

	for _, entry := range entries {
		if removed >= maxEntries {
			break
		}

		if err := ctx.Err(); err != nil {
			return reclaimed, err
		}

		name := entry.Name()
		if len(name) <= len("freeiran-") || name[:len("freeiran-")] != "freeiran-" {
			continue
		}

		if !entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if time.Since(info.ModTime()) < maxAge {
			continue
		}

		path := filepath.Join(root, name)

		size := runConfigSize(path)

		if err := os.RemoveAll(path); err == nil {
			reclaimed += size
			removed++
		}
	}

	return reclaimed, nil
}

// runtimeRootValue returns the configured value without fallback.
func runtimeRootValue() string {
	if value, ok := runtimeRoot.Load().(string); ok {
		return value
	}

	return ""
}

// runConfigSize sums file sizes under path (bounded accounting walk).
func runConfigSize(path string) int64 {
	var total int64

	_ = filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if info, err := entry.Info(); err == nil && entry.Type().IsRegular() {
			total += info.Size()
		}

		return nil
	})

	return total
}
