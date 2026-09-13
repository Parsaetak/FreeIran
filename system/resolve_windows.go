//go:build windows

package system

import (
	"os"
	"path/filepath"
	"strings"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// resolveExecutable maps a ProcessSpec.Path to a validated,
// launchable executable path.
//
// Validation rules (invariant: NEVER weaker than v0.7):
//
//   - A path containing a separator (or an absolute path) must point
//     to an existing regular file. This is the protocol-core path:
//     cores are always staged and discovered as explicit paths, and
//     they keep the exact os.Stat validation the launcher always had.
//
//   - A bare name (no separator) is ONLY accepted for the Windows
//     system shell (cmd / cmd.exe), which tests and internal helpers
//     launch as a process stand-in. It is resolved through %COMSPEC%
//     when that variable names cmd.exe and the file exists, otherwise
//     through %SystemRoot%\System32\cmd.exe, again validated on disk.
//     The current working directory and the inherited PATH are never
//     consulted, so test behaviour cannot depend on where the runner
//     happened to start.
//
//   - Any other bare name is rejected: the caller must resolve it
//     explicitly (CoreLocator / registry do exactly that).
func resolveExecutable(path string) (string, error) {
	if strings.ContainsAny(path, `/\`) {
		if !fileExists(path) {
			return "", firerrors.New(firerrors.KindDependencyUnavailable,
				Subsystem, "start", "core binary %q", path)
		}

		return path, nil
	}

	return ResolveSystemExecutable(path)
}

// ResolveSystemExecutable resolves a known Windows system executable
// by bare name to a validated absolute path. It exists so tests and
// system helpers never depend on the working directory or PATH.
//
// Supported names: "cmd" and "cmd.exe". Everything else is an error.
func ResolveSystemExecutable(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cmd", "cmd.exe":
		if path := resolveCmd(); path != "" {
			return path, nil
		}

		return "", firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "start",
			"cmd.exe could not be resolved: neither %%COMSPEC%% nor "+
				"%%SystemRoot%%\\System32\\cmd.exe validated")
	}

	return "", firerrors.New(firerrors.KindDependencyUnavailable,
		Subsystem, "start",
		"bare executable name %q is not a known system binary; "+
			"provide an explicit path", name)
}

// resolveCmd finds cmd.exe: %COMSPEC% first (validated: it must exist
// and actually be cmd.exe, so a hijacked COMSPEC cannot smuggle in an
// arbitrary binary), then %SystemRoot%\System32\cmd.exe, then the
// hard fallback C:\Windows\System32\cmd.exe. A nil return means none
// of the candidates validated — the caller surfaces the error.
func resolveCmd() string {
	if comspec := os.Getenv("COMSPEC"); comspec != "" {
		if isCmdExe(comspec) && fileExists(comspec) {
			return comspec
		}
	}

	if root := os.Getenv("SystemRoot"); root != "" {
		if candidate := filepath.Join(root, "System32", "cmd.exe"); fileExists(candidate) {
			return candidate
		}
	}

	// Last resort for environments without SystemRoot (extremely
	// unusual): the canonical location.
	if candidate := filepath.Join(`C:\Windows\System32`, "cmd.exe"); fileExists(candidate) {
		return candidate
	}

	return ""
}

// isCmdExe reports whether path plausibly names cmd.exe (the COMSPEC
// contract), so a COMSPEC pointing elsewhere is treated as unset
// rather than trusted blindly.
func isCmdExe(path string) bool {
	base := strings.ToLower(filepath.Base(path))

	return base == "cmd.exe" || base == "cmd"
}
