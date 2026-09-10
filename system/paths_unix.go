//go:build !windows

package system

import (
	"os"
	"path/filepath"
)

// DefaultBaseDir returns the platform-appropriate application data
// directory for FreeIran. It respects XDG on Unix-like systems.
func DefaultBaseDir() string {
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return filepath.Join(dataHome, "FreeIran")
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "FreeIran")
	}

	return filepath.Join(os.TempDir(), "FreeIran")
}

// CacheBaseDir returns the platform cache directory root.
func CacheBaseDir() string {
	if cacheHome := os.Getenv("XDG_CACHE_HOME"); cacheHome != "" {
		return filepath.Join(cacheHome, "FreeIran")
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "FreeIran")
	}

	return filepath.Join(os.TempDir(), "FreeIran")
}
