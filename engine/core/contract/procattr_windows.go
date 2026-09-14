//go:build windows

package contract

import (
	"os/exec"
	"syscall"
)

// hideConsole keeps test-launched protocol cores from flashing a CMD
// window on Windows CI/dev machines (v0.9.2 console audit: every
// exec.Command site applies hidden-window flags).
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000 | 0x00000200, // CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP
	}
}
