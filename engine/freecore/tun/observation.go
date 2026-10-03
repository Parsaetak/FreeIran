package tun

import (
	"net/netip"
	"time"
)

// Observation is the platform-neutral snapshot the IP Helper seam
// produces: interfaces with their addresses, and the forwarding
// table. READ-ONLY facts — the seam never mutates network state (no
// netsh, no route.exe, no PowerShell; the architectural rule).
type Observation struct {
	Interfaces  []InterfaceFact
	Routes      []RouteFact
	CollectedAt time.Time
}

// InterfaceFact is one observed network interface.
type InterfaceFact struct {
	Name      string
	Index     uint32
	Addresses []netip.Prefix
	Running   bool
}

// RouteFact is one observed forwarding-table row (IPv4).
type RouteFact struct {
	Prefix         netip.Prefix
	InterfaceIndex uint32
	Gateway        netip.Addr
}

// FindInterface returns the interface with the exact name (the
// deterministic-identity lookup — no fuzzy matching).
func (o Observation) FindInterface(name string) (InterfaceFact, bool) {
	for _, iface := range o.Interfaces {
		if iface.Name == name {
			return iface, true
		}
	}

	return InterfaceFact{}, false
}

// FindInterfaceByIndex returns the interface with the exact index.
func (o Observation) FindInterfaceByIndex(index uint32) (InterfaceFact, bool) {
	for _, iface := range o.Interfaces {
		if iface.Index == index {
			return iface, true
		}
	}

	return InterfaceFact{}, false
}

// DefaultInterfaceHints returns the non-TUN interfaces carrying a
// default route (0.0.0.0/0), best first — the physical-side
// candidates the future upstream dialer binds to. The excludeIndex
// is the TUN interface index (loop prevention).
func (o Observation) DefaultInterfaceHints(excludeIndex uint32) []InterfaceFact {
	var out []InterfaceFact

	for _, route := range o.Routes {
		if route.InterfaceIndex == excludeIndex {
			continue
		}

		if route.Prefix != netip.MustParsePrefix("0.0.0.0/0") {
			continue
		}

		if iface, ok := o.FindInterfaceByIndex(route.InterfaceIndex); ok && iface.Running {
			out = append(out, iface)
		}
	}

	return out
}

// CoveringRoutesHeld reports whether the TUN interface owns every
// covered prefix (the route-observation verdict the Phase 2
// activation gate needs: a probe that succeeds while the default
// route still points elsewhere is not evidence).
func (o Observation) CoveringRoutesHeld(guard LoopGuard) bool {
	for _, covered := range guard.CoveredRoutes {
		held := false

		for _, route := range o.Routes {
			if route.Prefix == covered && route.InterfaceIndex == guard.InterfaceIndex {
				held = true

				break
			}
		}

		if !held {
			return false
		}
	}

	return len(guard.CoveredRoutes) > 0
}

// FreshWithin reports whether the observation is recent enough to
// act on (stale facts must fail closed, not guess).
func (o Observation) FreshWithin(maxAge time.Duration, now time.Time) bool {
	if o.CollectedAt.IsZero() {
		return false
	}

	return now.Sub(o.CollectedAt) <= maxAge
}
