//go:build !windows

package httpx

import "errors"

// classifyRenameRetryable reports whether a rename failure is worth
// retrying. On non-Windows platforms renames over an existing
// destination are atomic and share-locking does not exist, so ONLY
// the test sentinel qualifies — production errors return immediately
// (retrying would only delay a real failure).
func classifyRenameRetryable(err error) bool {
	return err != nil && errors.Is(err, errStillLocked)
}
