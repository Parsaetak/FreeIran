package system

import (
	"os"
	"path/filepath"
)

// portableRoot returns the executable directory when it hosts a
// portable deployment layout (a `portable.marker` file or a `config`
// directory next to the executable), or "" otherwise.
//
// Since v0.9.2 the executable directory is ALWAYS the workspace root,
// so this helper no longer influences path resolution; it only answers
// "was this a shipped portable deployment?" for diagnostics and the
// developer settings surface.
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

// PortableMode reports whether the application is running from a
// portable deployment (v0.9.0 layout): a config directory or a
// portable.marker file sits next to the running executable. The path
// implementations consulted this condition when resolving the base
// directory; since v0.9.2 every deployment is workspace-rooted, so
// this helper only labels the deployment style for diagnostics and
// the developer settings surface.
//
// The check is read-only and safe to call at any time.
func PortableMode() bool {
	return portableRoot() != ""
}
