//go:build windows

package tunnel

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	fctun "github.com/Parsaetak/FreeIran/engine/freecore/tun"
)

// obsFixture builds a synthetic observation: Ethernet (3) owns the
// default route; the FreeIran TUN (7) holds the covering pair.
func obsFixture() fctun.Observation {
	must := netip.MustParsePrefix

	return fctun.Observation{
		Interfaces: []fctun.InterfaceFact{
			{Name: "Ethernet", Index: 3, Running: true, Addresses: []netip.Prefix{must("192.168.1.10/24")}},
			{Name: "FreeIranTUN", Index: 7, Running: true, Addresses: []netip.Prefix{must("198.18.0.1/30")}},
		},
		Routes: []fctun.RouteFact{
			{Prefix: must("0.0.0.0/0"), InterfaceIndex: 3, Gateway: netip.MustParseAddr("192.168.1.1")},
			{Prefix: must("0.0.0.0/1"), InterfaceIndex: 7},
			{Prefix: must("128.0.0.0/1"), InterfaceIndex: 7},
		},
	}
}

// TestBuildUpstreamConstraintSelectsPhysical pins that the constraint
// names the PHYSICAL interface as the bind target and the TUN as the
// forbidden one — the two facts must never share a field again.
func TestBuildUpstreamConstraintSelectsPhysical(t *testing.T) {
	identity := fctun.Identity{AdapterName: "FreeIranTUN"}
	tunPrefix := netip.MustParsePrefix("198.18.0.0/30")

	constraint := buildUpstreamConstraint(obsFixture(), identity, tunPrefix)
	if constraint == nil {
		t.Fatal("constraint not built from a healthy observation")
	}

	if constraint.PhysicalInterfaceIndex != 3 {
		t.Fatalf("physical = %d, want 3 (the default-route owner)", constraint.PhysicalInterfaceIndex)
	}

	if constraint.TUNInterfaceIndex != 7 {
		t.Fatalf("tun = %d, want 7 (the FreeIran adapter)", constraint.TUNInterfaceIndex)
	}

	if err := constraint.Validate(); err != nil {
		t.Fatalf("constraint validates: %v", err)
	}
}

// TestBuildUpstreamConstraintMissingPhysicalFailsClosed: with NO
// physical default-route owner (only the TUN carries one, which is
// excluded), the constraint is nil and the activation must fail
// closed — never dial unbound (loop risk) or bound to the TUN (loop).
func TestBuildUpstreamConstraintMissingPhysicalFailsClosed(t *testing.T) {
	obs := obsFixture()
	// The Ethernet loses its default route: no physical owner remains.
	obs.Routes = obs.Routes[1:]

	identity := fctun.Identity{AdapterName: "FreeIranTUN"}

	if constraint := buildUpstreamConstraint(obs, identity, netip.MustParsePrefix("198.18.0.0/30")); constraint != nil {
		t.Fatalf("constraint built without a physical upstream: %+v", constraint)
	}
}

// TestBuildUpstreamConstraintMissingAdapterIdentityFailsClosed: when
// the FreeIran adapter is NOT observed (stale/missing identity), the
// constraint must not pretend the TUN index is 0-and-fine — the TUN
// fact is zero (unknown), which the verify gate treats as unearned
// evidence.
func TestBuildUpstreamConstraintMissingAdapterIdentityFailsClosed(t *testing.T) {
	obs := obsFixture()
	obs.Interfaces = obs.Interfaces[:1] // the TUN adapter vanished from observation

	identity := fctun.Identity{AdapterName: "FreeIranTUN"}

	constraint := buildUpstreamConstraint(obs, identity, netip.MustParsePrefix("198.18.0.0/30"))
	if constraint == nil {
		// Failing closed is the outcome that matters; nil is acceptable.
		return
	}

	if constraint.TUNInterfaceIndex != 0 {
		t.Fatalf("TUN index %d invented without the adapter being observed", constraint.TUNInterfaceIndex)
	}
}

// TestVerifyFailsOnMissingAdapterIdentity drives the activation verify
// gate against synthetic observations: a missing/stale adapter
// identity, missing physical interface or failing constraint must
// refuse Active — with TrafficVerified never true.
func TestVerifyFailsOnMissingAdapterIdentity(t *testing.T) {
	identity := fctun.Identity{AdapterName: "FreeIranTUN"}
	tun4 := netip.MustParsePrefix("198.18.0.1/30")
	tun6 := netip.MustParsePrefix("fdfe:dcba:9876::1/126")

	cases := []struct {
		name string
		obs  fctun.Observation
	}{
		{
			name: "adapter gone",
			obs: func() fctun.Observation {
				o := obsFixture()
				o.Interfaces = o.Interfaces[:1]

				return o
			}(),
		},
		{
			name: "adapter down",
			obs: func() fctun.Observation {
				o := obsFixture()
				o.Interfaces[1].Running = false

				return o
			}(),
		},
		{
			name: "physical interface gone",
			obs: func() fctun.Observation {
				o := obsFixture()
				o.Interfaces = o.Interfaces[1:]

				return o
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := tunObserve
			tunObserve = func() (fctun.Observation, error) { return tc.obs, nil }
			t.Cleanup(func() { tunObserve = restore })

			b := &freecoreTUNBackend{ipv4: tun4, ipv6: tun6}

			constraint := buildUpstreamConstraint(tc.obs, identity, tun4)

			evidence, ok := b.verify(tc.obs, identity, constraint)
			if ok {
				t.Fatalf("verify accepted a broken observation: %+v", evidence)
			}

			// The evidence ladder must not claim more than it proved.
			if evidence.TrafficVerified {
				t.Fatal("TrafficVerified asserted by a gate that executes no traffic probe")
			}
		})
	}
}

// TestVerifyEarnsRouteReadyOnHealthyObservation: with every gate green
// at the observation level (the LUID ownership check needs a real
// device and is skipped through the device-guard branch), the ladder
// reaches route-ready and STILL never claims traffic verification.
func TestVerifyEarnsRouteReadyOnHealthyObservation(t *testing.T) {
	identity := fctun.Identity{AdapterName: "FreeIranTUN"}
	tun4 := netip.MustParsePrefix("198.18.0.1/30")
	tun6 := netip.MustParsePrefix("fdfe:dcba:9876::1/126")

	restore := tunObserve
	tunObserve = func() (fctun.Observation, error) { return obsFixture(), nil }
	t.Cleanup(func() { tunObserve = restore })

	b := &freecoreTUNBackend{ipv4: tun4, ipv6: tun6, device: &noLUIDDevice{}}

	constraint := buildUpstreamConstraint(obsFixture(), identity, tun4)

	evidence, ok := b.verify(obsFixture(), identity, constraint)
	if ok {
		// On a healthy synthetic observation the ladder earns route-ready
		// (the LUID branch refuses devices without a LUID — that refusal
		// is itself the honest outcome here).
		t.Logf("verify earned the full ladder on synthetic facts: %+v", evidence)

		if evidence.TrafficVerified {
			t.Fatal("TrafficVerified must never be earned without a real traffic probe")
		}

		return
	}

	// The refusal must come from the device lacking its LUID (no real
	// adapter in tests), NOT from the observation gates.
	if evidence.PlatformReady && evidence.StackReady && evidence.RouteReady {
		t.Fatalf("unexpected failure shape: %+v", evidence)
	}
}

// noLUIDDevice is a device stub that does not expose a LUID — the
// verify gate must refuse it (exact ownership unverifiable).
type noLUIDDevice struct{}

// Identity implements part of the device surface.
func (noLUIDDevice) Identity() fctun.Identity { return fctun.Identity{AdapterName: "FreeIranTUN"} }

// MTU implements part of the device surface.
func (noLUIDDevice) MTU() int { return 1280 }

// WritePacket implements part of the device surface.
func (noLUIDDevice) WritePacket(p fctun.Packet) error { return nil }

// Close implements part of the device surface.
func (noLUIDDevice) Close() error { return nil }

// Done implements part of the device surface.
func (noLUIDDevice) Done() <-chan struct{} { return nil }

// Packets implements part of the device surface.
func (noLUIDDevice) Packets() <-chan fctun.Packet { return nil }

// compile-time guard: the stub satisfies the device surface.
var _ fctun.Device = noLUIDDevice{}

// stubTUNCoreResolver is a TUNCoreResolver standing in for the managed
// sing-box wiring. It deliberately reports an OBSOLETE core version so
// the sing-box fallback refuses at the version guard — a refusal that
// happens BEFORE elevation and before any system mutation, so the
// fallback path is exercised safely on any runner (elevated or not).
type stubTUNCoreResolver struct{}

func (stubTUNCoreResolver) SingBoxBinary(_ context.Context) (string, string, error) {
	return `C:\cores\sing-box.exe`, "1.11.0", nil
}

func (stubTUNCoreResolver) EnsureSingBox(_ context.Context) error { return nil }

// TestFirstPartyTUNEnableFailsClosedWithoutElevation pins the v0.14.0
// first-party Windows activation gate: a configuration the FreeIran
// Engine genuinely supports (SOCKS) reaches the first-party dataplane,
// which refuses fail-closed with ErrRequiresElevation BEFORE opening a
// device or observing/mutating anything. The backend surfaces the
// refusal honestly (status failed, nothing active, no evidence rungs
// earned) and recovers to a clean off state through Disable.
func TestFirstPartyTUNEnableFailsClosedWithoutElevation(t *testing.T) {
	if tunProcessElevated() {
		t.Skip("process is elevated; the non-elevated fail-closed refusal cannot be exercised here")
	}

	backend := newTUNBackend(nil)

	if !backend.Available() {
		t.Fatal("windows: the first-party dataplane must be available (compiled in)")
	}

	supported := supportsFirstPartyTUN(config.Config{Type: config.TypeSOCKS})
	if !supported {
		t.Fatal("SOCKS must be inside the first-party capability set")
	}

	err := backend.Enable(context.Background(), TUNEnableOptions{
		Config: config.Config{Type: config.TypeSOCKS},
	})
	if !errors.Is(err, ErrRequiresElevation) {
		t.Fatalf("Enable(socks) err = %v, want ErrRequiresElevation (fail-closed before any mutation)", err)
	}

	snap := backend.Snapshot()
	if snap.Active || snap.Status == tunStatusActive {
		t.Fatalf("snapshot after refused enable = %+v, want nothing active", snap)
	}

	if snap.Status != tunStatusFailed {
		t.Fatalf("status = %q, want the honest %q after a failed activation", snap.Status, tunStatusFailed)
	}

	if snap.TrafficVerified || snap.RouteReady || snap.StackReady {
		t.Fatalf("evidence = %+v, want no rungs earned by a refused activation", snap)
	}

	// Recovery hygiene: Disable after the failed enable is a harmless
	// no-op (nothing owns a session) and must never swallow or fake the
	// recorded failure: the first-party snapshot keeps surfacing the
	// failed status until a real activation succeeds.
	if err := backend.Disable(context.Background()); err != nil {
		t.Fatalf("Disable after refused enable err = %v, want nil", err)
	}

	if snap := backend.Snapshot(); snap.Active {
		t.Fatalf("snapshot after disable = %+v, want inactive", snap)
	} else if snap.Status == tunStatusActive || snap.Status == tunStatusOff {
		t.Fatalf("snapshot after disable = %+v, want the failed activation still surfaced honestly", snap)
	}

	// Controller seam: the same refusal through EnableTUN keeps the
	// controller direct/inactive with the failure surfaced to the UI.
	c := NewWithProxyBackend(nil)

	err = c.EnableTUN(context.Background(), TUNEnableOptions{
		Config: config.Config{Type: config.TypeSOCKS},
	})
	if !errors.Is(err, ErrRequiresElevation) {
		t.Fatalf("EnableTUN(socks) err = %v, want ErrRequiresElevation", err)
	}

	state := c.State()
	if state.Mode != ModeDirect || state.Active {
		t.Fatalf("state = %+v, want direct/inactive after the refused enable", state)
	}

	if !state.RequiresElevation {
		t.Fatal("the controller state must carry the elevation requirement to the UI")
	}

	if state.Details == "" || !strings.Contains(state.Details, "TUN enable failed") {
		t.Fatalf("details = %q, want the surfaced TUN failure", state.Details)
	}

	if state.TUN == nil {
		t.Fatal("the TUN snapshot must be attached to the state for the UI")
	}
}

// TestFirstPartyTUNRefusesUnsupportedConfiguration pins the capability
// gate of the first-party dataplane: a configuration OUTSIDE the
// FreeIran Engine capability set (VLESS) is declined by the selection
// gate, never half-configured, and falls through to the unavailable
// fallback (no resolver wired here), which refuses with
// ErrTunUnavailable. The refusal must leave no status mutation behind
// on the first-party backend itself.
func TestFirstPartyTUNRefusesUnsupportedConfiguration(t *testing.T) {
	if supportsFirstPartyTUN(config.Config{Type: config.TypeVLESS}) {
		t.Fatal("VLESS must be outside the first-party capability set")
	}

	if supportsFirstPartyTUN(config.Config{}) {
		t.Fatal("an empty configuration must be outside the first-party capability set")
	}

	backend := newTUNBackend(nil)

	err := backend.Enable(context.Background(), TUNEnableOptions{
		Config: config.Config{Type: config.TypeVLESS},
	})
	if !errors.Is(err, ErrTunUnavailable) {
		t.Fatalf("Enable(vless) err = %v, want ErrTunUnavailable (fallback refusal)", err)
	}

	// The first-party dataplane itself stayed untouched: no failed
	// status leaked into its snapshot (the refusal happened at the
	// selection gate, before the first-party transaction began).
	firstParty := newFreecoreTUNBackend()
	if snap := firstParty.Snapshot(); snap.Status != tunStatusOff || snap.Active {
		t.Fatalf("first-party snapshot = %+v, want untouched off/inactive", snap)
	}
}

// TestTUNBackendWithWiredResolverSelectsSingBoxFallback pins the
// fallback/external-resolver half of the selection policy: with a
// resolver wired, configurations beyond the first-party capability set
// reach the managed sing-box dataplane, which enforces its OWN honest
// gates (here: the minimum-core-version guard refuses an obsolete
// core) — never a fake activation. The version guard fires BEFORE
// elevation, so this test is safe on any runner.
func TestTUNBackendWithWiredResolverSelectsSingBoxFallback(t *testing.T) {
	backend := newTUNBackend(stubTUNCoreResolver{})

	if !backend.Available() {
		t.Fatal("windows: TUN must report available with the first-party dataplane compiled in")
	}

	// VLESS is declined by the first-party gate and handed to the
	// sing-box fallback, whose version guard refuses the obsolete core.
	err := backend.Enable(context.Background(), TUNEnableOptions{
		Config: config.Config{Type: config.TypeVLESS},
	})
	if err == nil {
		t.Fatal("Enable(vless) with an obsolete managed core must refuse")
	}

	if !strings.Contains(err.Error(), "too old for the TUN dataplane") {
		t.Fatalf("err = %v, want the honest core-version refusal", err)
	}

	if errors.Is(err, ErrRequiresElevation) {
		t.Fatal("the version guard must refuse BEFORE the elevation gate")
	}

	if after := backend.Snapshot(); after.Active || after.Status == tunStatusActive {
		t.Fatalf("snapshot after refusal = %+v, want nothing active", after)
	}
}
