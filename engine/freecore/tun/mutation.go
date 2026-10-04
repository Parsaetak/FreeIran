package tun

import (
	"fmt"
	"net/netip"
)

// IPMutator is the additive OS IP-configuration seam of the first-party
// TUN transaction. Every operation is EXACT and ADDITIVE:
//
//   - AddAddress / DeleteAddress operate on ONE unicast address entry
//     (Create/DeleteUnicastIpAddressEntry) — never a family flush;
//   - AddRoute / DeleteRoute operate on ONE forwarding entry
//     (Create/DeleteIpForwardEntry2).
//
// The seam exists so the transactional contract is testable without an
// elevated Windows session AND so the production implementation cannot
// silently regress into family-wide flushes: the pre-0.14.1 code used
// SetIPAddressesForFamily, which FLUSHES the whole family on the
// interface before adding — destroying unrelated user-created
// addresses. That helper is banned from the activation path.
type IPMutator interface {
	// AddAddress adds one unicast address to the interface.
	AddAddress(p netip.Prefix) error

	// DeleteAddress deletes exactly that address entry.
	DeleteAddress(p netip.Prefix) error

	// AddRoute adds one forwarding entry through the interface.
	AddRoute(p netip.Prefix, nextHop netip.Addr, metric uint32) error

	// DeleteRoute deletes exactly that forwarding entry.
	DeleteRoute(p netip.Prefix, nextHop netip.Addr) error
}

// InterfaceConfig is one OS mutation batch for the adapter: the
// addresses to hold and the routes to install through the adapter's
// LUID (the Wintun device's own LUID — FreeIran-owned state only).
type InterfaceConfig struct {
	LUID        uint64
	Addresses   []netip.Prefix
	Routes      []netip.Prefix
	RouteMetric uint32
}

// ApplyInterfaceConfigWith performs the mutations through the given
// mutator and returns their undo closures in application order. The
// caller (Transaction.Rollback) runs them in reverse. A mid-batch
// failure leaves the returned undo list describing EXACTLY what was
// applied, so rollback stays complete — the transactional contract of
// the activation.
//
// Foreign state is untouchable by construction: only the exact entries
// this batch creates have inverses, and the undos remove only those.
func ApplyInterfaceConfigWith(mut IPMutator, cfg InterfaceConfig) ([]func() error, error) {
	var undo []func() error

	// 1. Unicast addresses — one ADDITIVE entry per address. No flush,
	//    no family wipe: pre-existing addresses (the user's own) are
	//    never in the blast radius.
	for _, p := range cfg.Addresses {
		if err := mut.AddAddress(p); err != nil {
			return undo, fmt.Errorf("freecore.tun: add address %s: %w", p, err)
		}

		prefix := p

		undo = append(undo, func() error {
			return mut.DeleteAddress(prefix)
		})
	}

	// 2. Routes through this interface only — one entry per prefix per
	//    family (the covered-routes model: 0.0.0.0/1 + 128.0.0.0/1 for
	//    IPv4, ::/1 + 8000::/1 for IPv6). The next hop is the
	//    family-appropriate unspecified address (on-link via the
	//    interface itself).
	for _, p := range cfg.Routes {
		nextHop := netip.IPv4Unspecified()
		if p.Addr().Is6() {
			nextHop = netip.IPv6Unspecified()
		}

		if err := mut.AddRoute(p, nextHop, cfg.RouteMetric); err != nil {
			return undo, fmt.Errorf("freecore.tun: add route %s: %w", p, err)
		}

		prefix, hop := p, nextHop

		undo = append(undo, func() error {
			return mut.DeleteRoute(prefix, hop)
		})
	}

	return undo, nil
}
