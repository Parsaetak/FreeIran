package tunnel

import (
	"context"
	"errors"
)

// tun_unavailable.go implements the honest capability-limited TUN
// backend used when the real dataplane cannot run:
//
//   - no TUNCoreResolver is wired (the application layer was not
//     given the managed-core access it needs), or
//   - the platform has no TUN implementation yet (v0.11.3 ships
//     Windows; see tun_other.go).
//
// The v0.9.8.5 backend it replaces was removed because its
// implementation could not be made trustworthy: raw curl/PowerShell
// Wintun acquisition with no digest, netsh/route shell mutations,
// inverted route bookkeeping, non-transactional DNS "restore",
// unbounded extraction. NONE of those patterns returned — v0.11.3
// implements TUN through the managed sing-box core (tun.go), and
// this backend remains only as the honest refusal for hosts that
// cannot serve it.

// unavailableTUNBackend reports TUN as unavailable with the exact
// reason, never with fake support.
type unavailableTUNBackend struct{}

// Available reports false: this host cannot serve TUN sessions.
func (unavailableTUNBackend) Available() bool { return false }

// Install refuses: there is no dependency to install without the
// managed-core wiring (the Wintun dependency travels INSIDE the
// verified sing-box binary — see tun.go).
func (unavailableTUNBackend) Install(ctx context.Context) error {
	return ErrTunUnavailable
}

// Enable refuses before touching anything: no platform/dataplane is
// available on this host.
func (unavailableTUNBackend) Enable(ctx context.Context, opts TUNEnableOptions) error {
	return ErrTunUnavailable
}

// Disable is a no-op: nothing can be enabled, so nothing needs
// tearing down.
func (unavailableTUNBackend) Disable(ctx context.Context) error { return nil }

// Snapshot reports the honest state: not available, not installed,
// elevation required (TUN is an elevated feature wherever it exists).
func (unavailableTUNBackend) Snapshot() TUNSnapshot {
	return TUNSnapshot{
		Available:         false,
		Installed:         false,
		RequiresElevation: true,
		Backend:           tunBackendName,
		Status:            tunStatusOff,
		Details:           ErrTunUnavailable.Error(),
	}
}

// newTUNBackend returns the platform TUN backend with the v0.14.0
// selection policy: the FIRST-PARTY dataplane (when the platform
// provides one) is preferred for configurations the FreeIran Engine
// genuinely supports; the managed sing-box dataplane remains the
// compatibility fallback for everything else; without a wired core
// resolver the unavailable backend reports honestly.
func newTUNBackend(resolver TUNCoreResolver) TUNBackend {
	var fallback TUNBackend = unavailableTUNBackend{}

	if resolver != nil {
		fallback = newSingboxTUNBackend(resolver)
	}

	return newSelectingTUNBackend(newFreecoreTUNBackend(), fallback)
}

// errTunNotWired is retained as a sentinel for callers probing WHY
// TUN is unavailable in tests.
var errTunNotWired = errors.New("tunnel: TUN core resolver is not wired")
