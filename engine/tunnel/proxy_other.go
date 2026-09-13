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

// newSystemProxyBackend returns a stub on non-Windows platforms.
func newSystemProxyBackend() SystemProxyBackend {
	return stubSystemProxyBackend{}
}
