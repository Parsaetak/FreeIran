//go:build windows

package system

import (
	"os"
	"path/filepath"
)

// DefaultBaseDir returns the Windows application data directory for
// FreeIran (%APPDATA%\FreeIran).
func DefaultBaseDir() string {
	if appData := os.Getenv("APPDATA"); appData != "" {
		return filepath.Join(appData, "FreeIran")
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "AppData", "Roaming", "FreeIran")
	}

	return filepath.Join(os.TempDir(), "FreeIran")
}

// CacheBaseDir returns the Windows cache directory root.
func CacheBaseDir() string {
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		return filepath.Join(localAppData, "FreeIran")
	}

	return DefaultBaseDir()
}
