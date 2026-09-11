//go:build !windows

package system

import (
	"os/exec"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// OpenDirectory reveals a directory in the platform file manager
// (XDG open on Linux and other Unix-like systems).
func OpenDirectory(path string) error {
	if path == "" {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "open", "path is empty")
	}

	return exec.Command("xdg-open", path).Start()
}
