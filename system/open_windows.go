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
	// the process is only started, never waited for.
	return exec.Command("explorer", path).Start()
}
