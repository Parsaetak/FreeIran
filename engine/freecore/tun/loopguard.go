package tun

import (
	"errors"
	"net/netip"
)

// Fail-closed constraint errors: a caller that cannot name the
// physical interface (or that tries to bind upstream traffic to the
// TUN) gets a loud refusal, never a silent misbind.
var (
	errMissingPhysicalInterface = errors.New("tun: upstream constraint has no physical interface (refusing to dial unbound)")
	errPhysicalIsTUN            = errors.New("tun: upstream constraint binds the TUN interface (loop refusal)")
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

// UpstreamConstraint is the machine-readable dial constraint for the
// engine-side upstream dialer. The v0.14.1 model makes the semantics
// EXPLICIT — the pre-0.14.1 single ExcludedInterfaceIndex field was
// documented as "the TUN interface to avoid" while every producer
// populated it with the PHYSICAL interface index and the dialer bound
// TO that value: one field, two opposite meanings, and a latent path
// where an upstream socket could be bound to the TUN itself.
//
// The API now names both facts:
//
//   - TUNInterfaceIndex: the FreeIran adapter. Recorded for
//     diagnostics and loop assertions; NEVER a valid bind target.
//   - PhysicalInterfaceIndex: the interface upstream sockets MUST be
//     bound to (the live-observed default-route owner). Zero means
//     "unknown" — and the dialer fails closed on unknown.
type UpstreamConstraint struct {
	// TUNInterfaceIndex is the FreeIran TUN interface index in this
	// session. Upstream sockets must never bind to it and must never
	// be sourced from its addresses.
	TUNInterfaceIndex uint32

	// PhysicalInterfaceIndex is the interface the OS currently routes
	// the default through (excluding the TUN). The constrained dialer
	// binds upstream sockets here.
	PhysicalInterfaceIndex uint32

	// ForbiddenSourcePrefixes are the address spaces an upstream
	// socket must never be sourced from (the TUN-local plan).
	ForbiddenSourcePrefixes []netip.Prefix
}

// Validate enforces the constraint's own invariants:
//
//   - a physical interface must be known (zero fails closed);
//   - the physical interface must never be the TUN interface (the
//     exact misbind the old single-field model allowed silently).
func (c UpstreamConstraint) Validate() error {
	if c.PhysicalInterfaceIndex == 0 {
		return errMissingPhysicalInterface
	}

	if c.TUNInterfaceIndex != 0 && c.PhysicalInterfaceIndex == c.TUNInterfaceIndex {
		return errPhysicalIsTUN
	}

	return nil
}

// Constraint renders the LoopGuard as the dialer constraint. The TUN
// interface index comes from the guard (a fact the guard owns); the
// PHYSICAL interface index is the caller's separately-observed fact —
// the guard deliberately does NOT guess it, so a caller that forgot to
// observe the physical interface gets a fail-closed constraint (zero),
// never a silent bind to the TUN.
func (g LoopGuard) Constraint(physicalInterfaceIndex uint32) UpstreamConstraint {
	return UpstreamConstraint{
		TUNInterfaceIndex:       g.InterfaceIndex,
		PhysicalInterfaceIndex:  physicalInterfaceIndex,
		ForbiddenSourcePrefixes: append([]netip.Prefix(nil), g.TunAddresses...),
	}
}

// DefaultCoveredRoutes returns the canonical covering-route set for a
// full-tunnel default route: the IPv4 pair (0.0.0.0/1 + 128.0.0.0/1)
// and the IPv6 pair (::/1 + 8000::/1) — the split-default shape that
// overrides the OS default of each family without deleting it. The
// first-party TUN assigns an IPv6 address and must own the SAME
// covering authority for IPv6, or it cannot honestly claim dual-stack
// routing.
func DefaultCoveredRoutes() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/1"),
		netip.MustParsePrefix("128.0.0.0/1"),
		netip.MustParsePrefix("::/1"),
		netip.MustParsePrefix("8000::/1"),
	}
}
