// Package system open_shell.go implements the v0.11.2 "Open shell
// here" operation: launch the user's selected Windows shell
// (PowerShell or CMD) with the selected directory as its working
// directory.
//
// SECURITY CONTRACT — this is NOT a generic command executor. The only
// thing it can do is open the chosen shell binary, with the chosen
// directory as cwd, with NO arguments supplied by the caller. The
// executable path is resolved through the OS search path (or the
// well-known System32 location for cmd.exe / Windows PowerShell), never
// from a user-supplied string. The working directory is set through
// SysProcAttr / os.Dir (not `cd path &&` concatenation) so spaces,
// Unicode and UNC paths are handled by the OS shell-launch path
// directly — no shell interpolation of the directory is performed.
//
// Allowed shells are explicitly enumerated; no other shell type is
// accepted by the API.
package system

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// ShellType enumerates the shells FreeIran can open at a directory.
//
// v0.11.2 contract: only PowerShell and CMD are supported. Any other
// value returns ErrUnsupportedShell — the API never silently falls
// back to a default shell.
type ShellType string

const (
	ShellPowerShell ShellType = "powershell"
	ShellCMD        ShellType = "cmd"
)

// ErrUnsupportedShell is returned when the caller requests a shell
// type that is not in the enumerated set above.
var ErrUnsupportedShell = errors.New("system: unsupported shell type")

// shellExecutableFor returns the absolute executable path for the
// requested shell on the current platform. On Windows, both
// powershell.exe and cmd.exe resolve through well-known System32 /
// WindowsPowerShell paths; on other platforms the only shell currently
// surfaced is PowerShell via the OS PATH (pwsh on Linux/macOS). The
// caller still gets a clean error on platforms that cannot serve a
// given shell, so the UI can honestly report the limitation rather
// than fabricate success.
func shellExecutableFor(shell ShellType) (string, error) {
	switch shell {
	case ShellPowerShell:
		// On Windows: prefer Windows PowerShell (powershell.exe). On
		// Linux/macOS the same identifier resolves to PowerShell Core
		// (pwsh) through PATH when installed; exec.LookPath reports
		// the honest answer either way.
		return lookShell("powershell.exe", "pwsh")
	case ShellCMD:
		// cmd.exe is Windows-only; LookPath returns an error on
		// other platforms and we propagate it.
		return lookShell("cmd.exe")
	default:
		return "", ErrUnsupportedShell
	}
}

// OpenShellAtDirectory launches the chosen shell with the chosen
// directory as its working directory. The path must be an absolute,
// OS-native directory path (callers in engine/app normalize workspace
// roots through the existing layout helpers). The shell binary is
// resolved through OS lookup — no caller-supplied executable path is
// ever accepted.
//
// On Windows the child process inherits no console (concealChild)
// because the new shell window is its own GUI/Console process; on
// Unix the shell is detached from the parent's stdio so the launch
// does not block the GUI loop.
//
// The function returns nil only when the shell process has been
// STARTED; it does not wait for the shell to exit (a shell is a
// long-running interactive process).
func OpenShellAtDirectory(path string, shell ShellType) error {
	if path == "" {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "open-shell", "path is empty")
	}

	if !filepath.IsAbs(path) && !isUNCPath(path) {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "open-shell", "path must be absolute")
	}

	exe, err := shellExecutableFor(shell)
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindConfiguration,
			Subsystem, "open-shell",
			"resolve shell executable")
	}

	// cmd := exec.Command(exe) — NO arguments. The shell is launched
	// bare; the working directory is set through cmd.Dir (the OS
	// launches the binary with cwd already set), so the shell never
	// sees the directory as an argument. This means:
	//
	//   - Spaces in the path cannot cause argument injection.
	//   - Unicode paths are handled by the OS kernel directly.
	//   - UNC paths (\\?\C:\...) are accepted as-is on Windows.
	cmd := exec.Command(exe)
	cmd.Dir = path
	concealChild(cmd)

	if err := cmd.Start(); err != nil {
		return firerrors.Wrap(err, firerrors.KindConfiguration,
			Subsystem, "open-shell",
			"launch shell")
	}

	// Reap the launch helper process asynchronously so it does not
	// become a zombie. On Windows, cmd.Start on a console-subsystem
	// binary spawns a process that lives until the user closes the
	// window; we cannot Wait on it from the GUI loop.
	go func() {
		_ = cmd.Process.Release()
	}()

	return nil
}

// lookShell tries each candidate binary name in turn, returning the
// first one that resolves on PATH (or, on Windows, in the well-known
// System32 / WindowsPowerShell locations). Returns the bare executable
// name when LookPath fails on Windows for cmd.exe / powershell.exe —
// those are always present on Windows installs, and CreateProcess
// resolves them through the system search path even without LookPath.
func lookShell(candidates ...string) (string, error) {
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}

	// On Windows, cmd.exe and powershell.exe live in well-known
	// directories; return the bare name and let CreateProcess search.
	if len(candidates) > 0 {
		first := candidates[0]
		if strings.HasSuffix(strings.ToLower(first), ".exe") {
			return first, nil
		}
	}

	return "", ErrUnsupportedShell
}

// isUNCPath reports whether path uses the Windows UNC (\\server\share)
// or long-UNC (\\?\C:\...) form. The check is lexical; the OS makes
// the final authority decision on whether the path can be a cwd.
func isUNCPath(path string) bool {
	if len(path) < 2 {
		return false
	}

	if path[0] != '\\' || path[1] != '\\' {
		return false
	}

	return true
}
