//go:build windows

package system

import (
	"os/exec"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// OpenDirectory reveals a directory in Windows Explorer.
func OpenDirectory(path string) error {
	if path == "" {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "open", "path is empty")
	}

	// explorer.exe returns a non-zero exit code even on success, so
	// the process is only started, never waited for. It is launched
	// with the hidden-console flags for audit consistency (§v0.9.2):
	// explorer is a GUI-subsystem binary so this changes nothing for
	// the user, but every launch site follows the same rule so no
	// future console-subsystem helper can sneak in without flags.
	cmd := exec.Command("explorer", path)
	concealChild(cmd)

	return cmd.Start()
}
