//go:build !windows

package system

import (
	"os"
	"path/filepath"
)

// DefaultBaseDir returns the platform-appropriate application data
// directory for FreeIran. It respects XDG on Unix-like systems.
//
// v0.9.0 portable deployment: mirrors the Windows behavior — a
// deployment layout (config/ dir or portable.marker) next to the
// executable wins over the per-user location.
func DefaultBaseDir() string {
	if portable := portableRoot(); portable != "" {
		return portable
	}

	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return filepath.Join(dataHome, "FreeIran")
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "FreeIran")
	}

	return filepath.Join(os.TempDir(), "FreeIran")
}

// portableRoot returns the executable directory when it hosts a
// deployment layout, or "" when the regular per-user location applies.
func portableRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}

	dir := filepath.Dir(exe)

	if _, err := os.Stat(filepath.Join(dir, "portable.marker")); err == nil {
		return dir
	}

	if info, err := os.Stat(filepath.Join(dir, "config")); err == nil && info.IsDir() {
		return dir
	}

	return ""
}

// CacheBaseDir returns the platform cache directory root. Portable
// deployments keep their cache inside the deployment tree as well.
func CacheBaseDir() string {
	if portable := portableRoot(); portable != "" {
		return portable
	}

	if cacheHome := os.Getenv("XDG_CACHE_HOME"); cacheHome != "" {
		return filepath.Join(cacheHome, "FreeIran")
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "FreeIran")
	}

	return filepath.Join(os.TempDir(), "FreeIran")
}
