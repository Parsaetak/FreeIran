//go:build windows

package tun

import (
	"fmt"
	"net/netip"

	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// winip is the FreeIran-owned OS IP configuration surface for the
// first-party TUN transaction (Phase C). It wraps the pinned
// wireguard-windows winipcfg helpers (the same production-proven
// IP-Helper code path WireGuard uses) behind the additive IPMutator
// seam — whose result is a list of UNDO closures removing exactly the
// entries this session created.
//
// v0.14.1 — ADDITIVE ONLY (audited against the pinned winipcfg):
//
//   - LUID.AddIPAddress → CreateUnicastIpAddressEntry: adds ONE
//     unicast address, touching nothing else;
//   - LUID.DeleteIPAddress → DeleteUnicastIpAddressEntry: deletes
//     exactly that entry (OnLinkPrefixLength ignored by the API);
//   - LUID.AddRoute → CreateIpForwardEntry2 (dual-family
//     MIB_IPFORWARD_ROW2): adds ONE forwarding entry for either
//     address family;
//   - LUID.DeleteRoute → DeleteIpForwardEntry2: deletes exactly that
//     entry looked up through THIS interface's LUID.
//
// SetIPAddressesForFamily is deliberately NOT used anymore: it calls
// FlushIPAddresses(family) first, which removes EVERY address of that
// family on the interface — including unrelated user-created ones.
// A family-wide flush is not a transactional activation primitive.
//
// DNS settings are deliberately NOT exposed on this surface at all:
// mutating the Windows adapter DNS is out of contract for the
// first-party engine (docs/tun.md) and would create a second DNS
// authority.

// ipHelperMutator is the winipcfg-backed IPMutator over one LUID.
type ipHelperMutator struct {
	luid winipcfg.LUID
}

// AddAddress implements IPMutator (CreateUnicastIpAddressEntry).
func (m ipHelperMutator) AddAddress(p netip.Prefix) error {
	return m.luid.AddIPAddress(p)
}

// DeleteAddress implements IPMutator (DeleteUnicastIpAddressEntry —
// exactly this address entry, nothing else on the interface).
func (m ipHelperMutator) DeleteAddress(p netip.Prefix) error {
	return m.luid.DeleteIPAddress(p)
}

// AddRoute implements IPMutator (CreateIpForwardEntry2, dual-family).
func (m ipHelperMutator) AddRoute(p netip.Prefix, nextHop netip.Addr, metric uint32) error {
	return m.luid.AddRoute(p, nextHop, metric)
}

// DeleteRoute implements IPMutator (DeleteIpForwardEntry2 through this
// LUID — the route entry this interface owns).
func (m ipHelperMutator) DeleteRoute(p netip.Prefix, nextHop netip.Addr) error {
	return m.luid.DeleteRoute(p, nextHop)
}

// ApplyInterfaceConfig performs the mutations and returns their undo
// closures in application order (the transactional activation seam).
func ApplyInterfaceConfig(cfg InterfaceConfig) ([]func() error, error) {
	return ApplyInterfaceConfigWith(ipHelperMutator{luid: winipcfg.LUID(cfg.LUID)}, cfg)
}

// VerifyRoutesOwnedByLUID verifies that every covered prefix is owned
// by THIS interface (the dual-stack MIB_IPFORWARD_ROW2 lookup through
// the interface's own LUID — GetIpForwardEntry2 fails unless the entry
// exists for this LUID + destination + next hop). This is the IPv6
// ownership check the IPv4-table observation cannot provide, and the
// exact-ownership check for IPv4 on top of it: the TUN interface must
// hold its covering routes, not merely "the routes must exist".
func VerifyRoutesOwnedByLUID(luid uint64, covered []netip.Prefix) error {
	wluid := winipcfg.LUID(luid)

	for _, prefix := range covered {
		nextHop := netip.IPv4Unspecified()
		if prefix.Addr().Is6() {
			nextHop = netip.IPv6Unspecified()
		}

		if _, err := wluid.Route(prefix, nextHop); err != nil {
			return fmt.Errorf("freecore.tun: covered route %s not owned by this interface: %w", prefix, err)
		}
	}

	if len(covered) == 0 {
		return fmt.Errorf("freecore.tun: no covered routes to verify")
	}

	return nil
}
