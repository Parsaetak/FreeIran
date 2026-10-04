//go:build windows

package tun

import (
	"fmt"
	"net/netip"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// winip is the FreeIran-owned OS IP configuration surface for the
// first-party TUN transaction (Phase C). It wraps the wireguard-windows
// winipcfg helpers (the same production-proven IP-Helper code path
// WireGuard uses) behind ONE verb — apply a batch of interface
// mutations — whose result is a list of UNDO closures. DNS settings are
// deliberately NOT exposed on this surface at all: mutating the Windows
// adapter DNS is out of contract for the first-party engine
// (docs/tun.md) and would create a second DNS authority.

// InterfaceConfig is one OS mutation batch for the adapter: the
// addresses to hold and the routes to install through the adapter's
// LUID (the Wintun device's own LUID — FreeIran-owned state only).
type InterfaceConfig struct {
	LUID        uint64
	Addresses   []netip.Prefix
	Routes      []netip.Prefix
	RouteMetric uint32
}

// ApplyInterfaceConfig performs the mutations and returns their undo
// closures in application order. The caller (Transaction.Rollback) runs
// them in reverse. A mid-batch failure leaves the returned undo list
// describing EXACTLY what was applied, so rollback stays complete — the
// transactional contract of the activation.
func ApplyInterfaceConfig(cfg InterfaceConfig) ([]func() error, error) {
	luid := winipcfg.LUID(cfg.LUID)

	var undo []func() error

	// 1. Unicast addresses (per address — the undo mirrors the exact
	//    mutation, not a family-wide wipe that could touch foreign state).
	for _, p := range cfg.Addresses {
		if err := luid.SetIPAddressesForFamily(familyOf(p), []netip.Prefix{p}); err != nil {
			return undo, fmt.Errorf("freecore.tun: set address %s: %w", p, err)
		}

		prefix := p

		undo = append(undo, func() error {
			return luid.DeleteIPAddress(prefix)
		})
	}

	// 2. Routes through this interface only (never a gateway rewrite of
	//    the physical default route — the covered-routes model is
	//    0.0.0.0/1 + 128.0.0.0/1, the split-default shape).
	for _, p := range cfg.Routes {
		nextHop := netip.IPv4Unspecified()
		if p.Addr().Is6() {
			nextHop = netip.IPv6Unspecified()
		}

		if err := luid.AddRoute(p, nextHop, cfg.RouteMetric); err != nil {
			return undo, fmt.Errorf("freecore.tun: add route %s: %w", p, err)
		}

		prefix, hop := p, nextHop

		undo = append(undo, func() error {
			return luid.DeleteRoute(prefix, hop)
		})
	}

	return undo, nil
}

// familyOf maps a prefix to the Windows address family.
func familyOf(p netip.Prefix) winipcfg.AddressFamily {
	if p.Addr().Is4() {
		return windows.AF_INET
	}

	return windows.AF_INET6
}
