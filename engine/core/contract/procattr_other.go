//go:build !windows

package contract

import "os/exec"

// hideConsole is a no-op on non-Windows platforms (no console windows).
func hideConsole(cmd *exec.Cmd) {
	_ = cmd
}
