//go:build windows

package system

import (
	"os"
	"path/filepath"
)

// DefaultBaseDir returns the FreeIran base directory.
//
// v0.9.0 portable deployment: when the executable's directory contains
// a deployment layout (a `config` directory or a `portable.marker`
// file — both shipped in the official release ZIP), the whole tree
// lives next to the executable. Extract-and-run then retains
// configurations, cores and logs inside the deployment folder, which
// is exactly the "download ZIP → extract → launch → keep using the
// deployment" requirement. Otherwise the per-user %APPDATA%\FreeIran
// is used as before, and an existing installation is never affected.
func DefaultBaseDir() string {
	if portable := portableRoot(); portable != "" {
		return portable
	}

	if appData := os.Getenv("APPDATA"); appData != "" {
		return filepath.Join(appData, "FreeIran")
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "AppData", "Roaming", "FreeIran")
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

// CacheBaseDir returns the Windows cache directory root. Portable
// deployments keep their cache inside the deployment tree as well.
func CacheBaseDir() string {
	if portable := portableRoot(); portable != "" {
		return portable
	}

	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		return filepath.Join(localAppData, "FreeIran")
	}

	return DefaultBaseDir()
}
