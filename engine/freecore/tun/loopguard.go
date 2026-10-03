package tun

import (
	"net/netip"
)

// LoopGuard is the loop-prevention metadata the future first-party
// upstream dialer MUST honor (ROADMAP.md Phase 2 item 1): once the
// TUN owns the covering routes, any upstream connection that
// accidentally routes back through the TUN interface is a routing
// loop — the exact failure mode route.auto_detect_interface exists to
// prevent in the sing-box dataplane, represented here as first-class
// facts the engine-side dialer can be constrained and tested with.
type LoopGuard struct {
	// AdapterName is the FreeIran-owned adapter (exact identity).
	AdapterName string

	// InterfaceIndex is the observed interface index (LUID-derived
	// IfIndex) of the FreeIran adapter in THIS session.
	InterfaceIndex uint32

	// TunAddresses are the session's TUN-local addresses.
	TunAddresses []netip.Prefix

	// CoveredRoutes are the prefixes the TUN is expected to own
	// (0.0.0.0/0, or the 0.0.0.0/1 + 128.0.0.0/1 pair).
	CoveredRoutes []netip.Prefix
}

// Excludes reports whether an address must be dialed OUTSIDE the TUN
// (an upstream destination inside the covered space is a loop risk —
// the dialer must resolve and route it through the default physical
// interface instead).
func (g LoopGuard) Excludes(addr netip.Addr) bool {
	for _, prefix := range g.CoveredRoutes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// OwnsAddress reports whether an address is TUN-local (assigned to
// the FreeIran adapter).
func (g LoopGuard) OwnsAddress(addr netip.Addr) bool {
	for _, prefix := range g.TunAddresses {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// OwnsRoute reports whether a route belongs to the FreeIran TUN
// interface (used by the observation verdicts: the covering routes
// must point at the FreeIran interface, not merely exist).
func (g LoopGuard) OwnsRoute(prefix netip.Prefix, interfaceIndex uint32) bool {
	for _, covered := range g.CoveredRoutes {
		if covered == prefix && interfaceIndex == g.InterfaceIndex {
			return true
		}
	}

	return false
}

// UpstreamConstraint is the human/machine-readable dial constraint
// for the future engine-side upstream dialer: dial upstreams bound
// to a non-TUN interface; never source from a TUN-local address.
type UpstreamConstraint struct {
	// ExcludedInterfaceIndex is the TUN interface to avoid.
	ExcludedInterfaceIndex uint32

	// ForbiddenSourcePrefixes are the address spaces an upstream
	// socket must never be sourced from.
	ForbiddenSourcePrefixes []netip.Prefix
}

// Constraint renders the LoopGuard as the dialer constraint.
func (g LoopGuard) Constraint() UpstreamConstraint {
	return UpstreamConstraint{
		ExcludedInterfaceIndex:  g.InterfaceIndex,
		ForbiddenSourcePrefixes: append([]netip.Prefix(nil), g.TunAddresses...),
	}
}

// DefaultCoveredRoutes returns the canonical covering-route set for a
// full-tunnel default route (the 0.0.0.0/1 + 128.0.0.0/1 pair — the
// pair that overrides a 0.0.0.0/0 default without deleting it).
func DefaultCoveredRoutes() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/1"),
		netip.MustParsePrefix("128.0.0.0/1"),
	}
}
