package tunnel

import (
	"context"
	"errors"
	"fmt"
)

// tun_unavailable.go implements the v0.9.8.6 TUN policy: TUN mode is
// NOT production-ready and is deliberately NOT exposed as normal
// functionality. The previous Wintun backend was removed because its
// implementation could not be made trustworthy within the v0.9.8.6
// reliability/security bar:
//
//   - route bookkeeping was inverted (failures were recorded instead
//     of successes), so Disable could not reliably remove the routes
//     Enable had actually created;
//   - DNS "restoration" replaced the adapter's configuration with
//     DHCP instead of restoring the exact prior state — not
//     transactional, not reversible;
//   - Wintun acquisition used raw curl/PowerShell downloads with NO
//     authoritative SHA-256/signature verification — a remote
//     executable installed without integrity evidence;
//   - archive extraction ran through Expand-Archive with no size,
//     count or traversal bounds;
//   - interface/IP/DNS values were hardcoded instead of modelled
//     state;
//   - configuration mutated the system through netsh/route shell
//     calls instead of a controlled networking API;
//   - and fundamentally: TUN mode is NOT a kill switch. Process
//     supervision (job objects) keeps the core process from
//     outliving FreeIran; it does NOT filter packets. A crashed or
//     blocked route leaves the TUN adapter and its routes in place
//     until something cleans them up.
//
// Re-enabling TUN requires the full design to exist first: a
// transactional Enable (capture state → create → configure → mutate
// routes/DNS → verify → commit, with verified rollback of ONLY the
// successfully applied changes), authoritative Wintun acquisition
// (pinned digest, bounded extraction), a real route/DNS state model
// and honest documentation of its failure semantics. Until then this
// backend reports TUN as unavailable everywhere — visibly, honestly,
// and without faking support.

// ErrTunExperimental is the explicit, user-visible status of TUN mode
// in this release: unfinished, disabled, not a kill switch.
var ErrTunExperimental = errors.New(
	"tunnel: TUN mode is experimental and disabled in this release " +
		"(unverified Wintun acquisition and non-transactional route/DNS " +
		"mutation made it unsafe; it is not a kill switch)")

// unavailableTUNBackend reports TUN as unavailable on every platform.
type unavailableTUNBackend struct{}

// Available reports false: TUN is not exposed in this release.
func (unavailableTUNBackend) Available() bool { return false }

// Install refuses: the Wintun acquisition path no longer exists (it
// downloaded an executable without authoritative integrity evidence).
func (unavailableTUNBackend) Install(ctx context.Context) error {
	return fmt.Errorf("%w; Wintun installation is disabled until it can "+
		"be acquired with a pinned, verified digest", ErrTunExperimental)
}

// Enable refuses: Enable was not transactional (route tracking,
// DNS restore and rollback were all broken).
func (unavailableTUNBackend) Enable(ctx context.Context, host string, port int) error {
	return ErrTunExperimental
}

// Disable is a no-op: nothing can be enabled, so nothing needs
// tearing down.
func (unavailableTUNBackend) Disable(ctx context.Context) error { return nil }

// Snapshot reports the honest state: not available, not installed.
func (unavailableTUNBackend) Snapshot() TUNSnapshot {
	return TUNSnapshot{
		Available:         false,
		Installed:         false,
		RequiresElevation: true,
	}
}

// newTUNBackend returns the unavailable backend on every platform.
func newTUNBackend() TUNBackend { return unavailableTUNBackend{} }
