//go:build !windows

package tun

import "context"

// OpenDevice refuses the first-party device layer on non-Windows
// platforms with the honest capability error — never a fake adapter,
// never a silent no-op. (Phase 2 may grow platform devices; until
// then this is the truth.)
func OpenDevice(context.Context, Identity, DeviceOptions) (Device, error) {
	return nil, ErrUnsupportedPlatform
}
