package tun

import (
	"net/netip"
	"testing"
	"time"
)

func guardFixture() LoopGuard {
	return LoopGuard{
		AdapterName:    "FreeIran",
		InterfaceIndex: 7,
		TunAddresses: []netip.Prefix{
			netip.MustParsePrefix("172.19.0.2/32"),
		},
		CoveredRoutes: DefaultCoveredRoutes(),
	}
}

func TestLoopGuardExcludes(t *testing.T) {
	g := guardFixture()

	if !g.Excludes(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("every IPv4 address is inside the covered pair — must be excluded")
	}

	if !g.Excludes(netip.MustParseAddr("203.0.113.9")) {
		t.Fatal("high half must be excluded too")
	}
}

func TestLoopGuardOwnsAddress(t *testing.T) {
	g := guardFixture()

	if !g.OwnsAddress(netip.MustParseAddr("172.19.0.2")) {
		t.Fatal("TUN-local address must be owned")
	}

	if g.OwnsAddress(netip.MustParseAddr("172.19.0.3")) {
		t.Fatal("/32 assignment owns exactly one address")
	}
}

func TestLoopGuardOwnsRoute(t *testing.T) {
	g := guardFixture()

	if !g.OwnsRoute(netip.MustParsePrefix("0.0.0.0/1"), 7) {
		t.Fatal("covering route on the TUN interface must be owned")
	}

	if g.OwnsRoute(netip.MustParsePrefix("0.0.0.0/1"), 9) {
		t.Fatal("covering route on ANOTHER interface is not FreeIran's")
	}

	if g.OwnsRoute(netip.MustParsePrefix("10.0.0.0/8"), 7) {
		t.Fatal("non-covering route is not owned even on the TUN interface")
	}
}

func TestLoopGuardConstraint(t *testing.T) {
	// The two interface facts are DISTINCT: the guard carries the TUN's
	// own index; the physical interface is the caller's separately
	// observed fact supplied to Constraint.
	c := guardFixture().Constraint(9)

	if c.TUNInterfaceIndex != 7 {
		t.Fatalf("constraint TUN index = %d, want 7", c.TUNInterfaceIndex)
	}

	if c.PhysicalInterfaceIndex != 9 {
		t.Fatalf("constraint physical index = %d, want 9", c.PhysicalInterfaceIndex)
	}

	if len(c.ForbiddenSourcePrefixes) != 1 {
		t.Fatalf("forbidden source prefixes = %v", c.ForbiddenSourcePrefixes)
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("constraint {TUN 7, physical 9} must validate: %v", err)
	}
}

func TestLoopGuardConstraintFailsClosed(t *testing.T) {
	// Missing physical interface: the guard cannot guess it — the
	// constraint carries zero and MUST fail closed.
	missing := guardFixture().Constraint(0)
	if err := missing.Validate(); err == nil {
		t.Fatal("constraint without a physical interface validated (must fail closed)")
	}

	// Physical == TUN: the exact misbind the old single-field model
	// allowed silently. Now a loud refusal.
	misbind := guardFixture().Constraint(7)
	if err := misbind.Validate(); err == nil {
		t.Fatal("constraint binding the TUN as the physical interface validated (loop refusal missing)")
	}
}

func observationFixture() Observation {
	must := netip.MustParsePrefix

	return Observation{
		CollectedAt: time.Now().UTC(),
		Interfaces: []InterfaceFact{
			{Name: "Ethernet", Index: 3, Running: true, Addresses: []netip.Prefix{must("192.168.1.10/24")}},
			{Name: "FreeIran", Index: 7, Running: true, Addresses: []netip.Prefix{must("172.19.0.2/32")}},
			{Name: "Wi-Fi down", Index: 11, Running: false, Addresses: []netip.Prefix{must("10.1.0.4/16")}},
		},
		Routes: []RouteFact{
			{Prefix: must("0.0.0.0/0"), InterfaceIndex: 3, Gateway: netip.MustParseAddr("192.168.1.1")},
			{Prefix: must("0.0.0.0/1"), InterfaceIndex: 7},
			{Prefix: must("128.0.0.0/1"), InterfaceIndex: 7},
			{Prefix: must("10.1.0.0/16"), InterfaceIndex: 11},
		},
	}
}

func TestObservationFindInterface(t *testing.T) {
	obs := observationFixture()

	iface, ok := obs.FindInterface("FreeIran")
	if !ok || iface.Index != 7 {
		t.Fatalf("FindInterface(exact) = %+v, %v", iface, ok)
	}

	if _, ok := obs.FindInterface("freeiran"); ok {
		t.Fatal("interface lookup must be exact, never fuzzy")
	}

	if iface, ok := obs.FindInterfaceByIndex(3); !ok || iface.Name != "Ethernet" {
		t.Fatalf("FindInterfaceByIndex(3) = %+v, %v", iface, ok)
	}
}

func TestObservationDefaultInterfaceHints(t *testing.T) {
	obs := observationFixture()

	hints := obs.DefaultInterfaceHints(7)
	if len(hints) != 1 || hints[0].Name != "Ethernet" {
		t.Fatalf("default hints excluding TUN = %+v, want the running Ethernet", hints)
	}

	// Without exclusion the TUN-adjacent default still only lists the
	// physical default (the TUN owns /1s, not 0/0, in this fixture).
	hints = obs.DefaultInterfaceHints(0)
	if len(hints) != 1 {
		t.Fatalf("unexpected hints: %+v", hints)
	}
}

func TestObservationCoveringRoutesHeld(t *testing.T) {
	obs := observationFixture()

	if !obs.CoveringRoutesHeld(guardFixture()) {
		t.Fatal("both covering prefixes on the TUN interface = held")
	}

	// One prefix moved off the TUN: not held.
	broken := observationFixture()
	broken.Routes[1].InterfaceIndex = 3
	if broken.CoveringRoutesHeld(guardFixture()) {
		t.Fatal("covering route on another interface = NOT held")
	}

	// No covered routes configured: fail closed (never vacuously true).
	empty := LoopGuard{AdapterName: "FreeIran", InterfaceIndex: 7}
	if obs.CoveringRoutesHeld(empty) {
		t.Fatal("empty covered-route set must fail closed")
	}
}

func TestObservationFreshness(t *testing.T) {
	obs := observationFixture()
	now := time.Now().UTC()

	if !obs.FreshWithin(5*time.Second, now) {
		t.Fatal("fresh observation reported stale")
	}

	obs.CollectedAt = now.Add(-time.Minute)
	if obs.FreshWithin(5*time.Second, now) {
		t.Fatal("stale observation reported fresh")
	}

	obs.CollectedAt = time.Time{}
	if obs.FreshWithin(time.Hour, now) {
		t.Fatal("zero collection time must fail closed")
	}
}
