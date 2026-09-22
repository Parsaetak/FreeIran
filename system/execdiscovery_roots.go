package system

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// platformInstallPaths returns the bounded, platform-aware set of
// known executable paths for an engine — priority-3 discovery roots.
//
// INVARIANT (v0.9.14): these are CONTROLLED, BOUNDED probes built from
// known roots × known subdirectories × known executable names. There
// is deliberately NO filesystem walking here: a recursive scan of
// Program Files (or any tree) is prohibited — it would impose
// unpredictable startup latency and privacy-relevant full-disk
// visibility for no benefit over the known layouts.
//
// Every returned path is checked for existence by the caller before it
// becomes a candidate, and each candidate still passes the real
// version probe before it is validated.
func platformInstallPaths(spec EngineSpec) []string {
	switch runtime.GOOS {
	case "windows":
		return windowsInstallPaths(spec)
	case "darwin":
		return darwinInstallPaths(spec)
	default:
		return unixInstallPaths(spec)
	}
}

// windowsInstallPaths probes the known Windows application roots:
// %ProgramFiles%, %ProgramFiles(x86)%, %LocalAppData% (including its
// standard Programs subtree), %AppData% and %SystemDrive%\ProgramData
// — always as root × known-subdir × known-name, never a walk.
func windowsInstallPaths(spec EngineSpec) []string {
	roots := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("LocalAppData"),
		filepath.Join(os.Getenv("LocalAppData"), "Programs"),
		os.Getenv("AppData"),
		filepath.Join(os.Getenv("SystemDrive"), "ProgramData"),
	}

	return expandInstallPaths(roots, spec)
}

// darwinInstallPaths probes the standard macOS command-line roots
// (Homebrew Intel + Apple Silicon, /usr/local) plus per-user
// locations.
func darwinInstallPaths(spec EngineSpec) []string {
	home, _ := os.UserHomeDir()

	roots := []string{
		"/usr/local/bin",
		"/opt/homebrew/bin",
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, "bin"),
		"/opt",
		"/usr/local",
	}

	return expandInstallPaths(roots, spec)
}

// unixInstallPaths probes the standard Linux command-line roots:
// system bin dirs, /opt subtrees and per-user locations. It preserves
// the Windows-first architecture of the product while providing
// equivalent controlled coverage on Linux.
func unixInstallPaths(spec EngineSpec) []string {
	home, _ := os.UserHomeDir()

	roots := []string{
		"/usr/local/bin",
		"/usr/bin",
		"/usr/local/sbin",
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, "bin"),
		"/opt",
		"/usr/local",
	}

	return expandInstallPaths(roots, spec)
}

// expandInstallPaths crosses the root list with the engine's known
// subdirectories and binary names. For a bin-style root (…/bin) the
// executable is probed directly; for container roots (…/opt,
// %ProgramFiles%) each known subdirectory — and its bin/ child — is
// probed. The result is a small fixed list, deduplicated
// case-insensitively on Windows.
func expandInstallPaths(roots []string, spec EngineSpec) []string {
	seen := make(map[string]bool)

	paths := make([]string, 0, len(roots)*len(spec.Subdirs))

	add := func(p string) {
		if p == "" {
			return
		}

		key := p
		if runtime.GOOS == "windows" {
			key = strings.ToLower(p)
		}

		if seen[key] {
			return
		}

		seen[key] = true
		paths = append(paths, p)
	}

	for _, root := range roots {
		if root == "" {
			continue
		}

		isBinRoot := strings.EqualFold(filepath.Base(root), "bin")

		for _, name := range spec.binaryNames() {
			if isBinRoot {
				// Flat bin directory: the executable is expected
				// directly inside.
				add(filepath.Join(root, name))

				continue
			}

			for _, sub := range spec.Subdirs {
				add(filepath.Join(root, sub, name))
				add(filepath.Join(root, sub, "bin", name))
			}
		}
	}

	return paths
}
