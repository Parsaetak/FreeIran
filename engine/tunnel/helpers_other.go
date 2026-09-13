//go:build !windows

package tunnel

import (
	"context"
	"os"
)

// stubTUNBackend is a no-op on non-Windows platforms.
type stubTUNBackend struct{}

func (stubTUNBackend) Available() bool                   { return false }
func (stubTUNBackend) Install(ctx context.Context) error { return ErrUnsupportedPlatform }
func (stubTUNBackend) Enable(ctx context.Context, host string, port int) error {
	return ErrUnsupportedPlatform
}
func (stubTUNBackend) Disable(ctx context.Context) error { return nil }
func (stubTUNBackend) Snapshot() TUNSnapshot             { return TUNSnapshot{} }

// newTUNBackend returns a stub on non-Windows platforms.
func newTUNBackend() TUNBackend { return stubTUNBackend{} }

// Helpers used by the Windows build only; defined here so the
// Windows-only files (tun_windows.go, helpers_real.go) can reference
// them. On non-Windows these are unreachable at runtime because the
// callers are guarded by build tags.

func osGetEnvReal(key string) string              { return os.Getenv(key) }
func osStatReal(path string) (interface{}, error) { return os.Stat(path) }
func osRemoveReal(path string) error              { return os.Remove(path) }
func osExecutableReal() (string, error)           { return os.Executable() }
