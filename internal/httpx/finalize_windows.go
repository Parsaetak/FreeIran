package httpx

import (
	"errors"
	"syscall"
)

// classifyRenameRetryable reports whether a rename failure is a
// transient Windows sharing violation — another process (typically an
// antivirus or search indexer) briefly holding the file after we
// synced it. ONLY ERROR_SHARING_VIOLATION (32) qualifies: it is the
// unambiguously-transient case, and retrying anything else (access
// denied because of ACLs, invalid paths, missing directories) would
// mask real errors instead of absorbing a scanner's momentary lock.
func classifyRenameRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Test seam: the synthesized sentinel used on non-Windows
	// platforms by the unit tests.
	if errors.Is(err, errStillLocked) {
		return true
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.Errno(32) // ERROR_SHARING_VIOLATION
	}

	return false
}
