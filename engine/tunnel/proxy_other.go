//go:build !windows

package tunnel

import (
	"context"
)

// stubSystemProxyBackend is a no-op on non-Windows platforms.
type stubSystemProxyBackend struct{}

func (stubSystemProxyBackend) Enable(ctx context.Context, host string, port int, asHTTP bool, bypass []string) error {
	return ErrUnsupportedPlatform
}

func (stubSystemProxyBackend) Disable(ctx context.Context) error {
	return nil
}

func (stubSystemProxyBackend) Snapshot() SystemProxySnapshot {
	return SystemProxySnapshot{}
}

// Current reports the honest platform state: a non-Windows build
// cannot READ WinINet per-connection settings, so the transactional
// ownership flow refuses before any mutation.
func (stubSystemProxyBackend) Current() (SystemProxySnapshot, error) {
	return SystemProxySnapshot{}, ErrUnsupportedPlatform
}

// Restore reports the honest platform state: a non-Windows build
// cannot apply WinINet settings. The marker would never be written
// on this platform (Enable always fails), so this only fires for a
// workspace carried over from a Windows session.
func (stubSystemProxyBackend) Restore(previous SystemProxySnapshot) error {
	return ErrUnsupportedPlatform
}

// newSystemProxyBackend returns a stub on non-Windows platforms.
func newSystemProxyBackend() SystemProxyBackend {
	return stubSystemProxyBackend{}
}
