//go:build !windows

package system

import "syscall"

// ProcessAlive reports whether a process with the given pid is
// currently running. It is the cross-platform teardown proof used by
// lifecycle tests (v0.9.8.6): after a session teardown the supervised
// process must be gone, and this check says so directly instead of
// inferring it from file deletability.
//
// A pid of 0 or less is never alive. Process-id reuse can
// theoretically produce a false positive long after the original
// process exited; callers use it within bounded test windows where
// that race is not realistic.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	// Signal 0 performs the permission/existence check without
	// delivering any signal.
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}

	// ESRCH: no such process. EPERM: the process exists but belongs to
	// another user — still alive from the caller's perspective.
	return err == syscall.EPERM
}
