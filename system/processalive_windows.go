//go:build windows

package system

import (
	"syscall"
	"unsafe"
)

// ProcessAlive reports whether a process with the given pid is
// currently running. It is the cross-platform teardown proof used by
// lifecycle tests (v0.9.8.6): after a session teardown the supervised
// process must be gone, and this check says so directly instead of
// inferring it from file deletability.
//
// A pid of 0 or less is never alive.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	raw, _, _ := procOpenProcess.Call(uintptr(processQueryLimited), 0, uintptr(pid))
	if raw == 0 {
		// OpenProcess fails with ERROR_INVALID_PARAMETER for pids that
		// no longer exist.
		return false
	}

	defer closeHandle(syscall.Handle(raw))

	var exitCode uint32

	ok, _, _ := procGetExitCodeProcess.Call(
		uintptr(raw), uintptr(unsafe.Pointer(&exitCode)))

	// STILL_ACTIVE (0x103) is how a live process reports; any other
	// code is the process's actual exit code.
	return ok != 0 && exitCode == statusStillActive
}

// compile-time assertion that the syscall pieces used above exist in
// this package (they are declared in process_windows.go).
var _ = syscall.Handle(0)
