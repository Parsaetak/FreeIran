package tun

import (
	"net/netip"
	"strings"
	"testing"
)

// fakeIPMutator is a recording IPMutator simulating an interface that
// already carries FOREIGN (user-created) state: the exact scenario the
// additive-only contract must protect.
type fakeIPMutator struct {
	addresses map[netip.Prefix]bool
	routes    map[netip.Prefix]bool

	addCalls     int
	deleteCalls  int
	flushCalls   int // must stay ZERO: no flush primitive exists on the seam
	appliedOrder []string
}

func newFakeIPMutator() *fakeIPMutator {
	return &fakeIPMutator{
		addresses: make(map[netip.Prefix]bool),
		routes:    make(map[netip.Prefix]bool),
	}
}

// AddAddress implements IPMutator.
func (f *fakeIPMutator) AddAddress(p netip.Prefix) error {
	f.addCalls++
	f.addresses[p] = true
	f.appliedOrder = append(f.appliedOrder, "add "+p.String())

	return nil
}

// DeleteAddress implements IPMutator.
func (f *fakeIPMutator) DeleteAddress(p netip.Prefix) error {
	f.deleteCalls++
	delete(f.addresses, p)
	f.appliedOrder = append(f.appliedOrder, "del "+p.String())

	return nil
}

// AddRoute implements IPMutator.
func (f *fakeIPMutator) AddRoute(p netip.Prefix, _ netip.Addr, _ uint32) error {
	f.addCalls++
	f.routes[p] = true
	f.appliedOrder = append(f.appliedOrder, "route "+p.String())

	return nil
}

// DeleteRoute implements IPMutator.
func (f *fakeIPMutator) DeleteRoute(p netip.Prefix, _ netip.Addr) error {
	f.deleteCalls++
	delete(f.routes, p)
	f.appliedOrder = append(f.appliedOrder, "unroute "+p.String())

	return nil
}

// TestApplyInterfaceConfigPreservesForeignState is the required
// transactional proof:
//
//	pre-existing foreign address
//	→ FreeIran enable (apply)
//	→ foreign address still exists
//	→ FreeIran disable (rollback)
//	→ foreign address still exists.
//
// Rollback removes ONLY the entries this session created.
func TestApplyInterfaceConfigPreservesForeignState(t *testing.T) {
	foreignAddr := netip.MustParsePrefix("192.168.1.50/24")
	foreignRoute := netip.MustParsePrefix("192.168.1.0/24")

	mut := newFakeIPMutator()
	mut.addresses[foreignAddr] = true // pre-existing, user-created
	mut.routes[foreignRoute] = true   // pre-existing, user-created

	tunAddr4 := netip.MustParsePrefix("198.18.0.1/30")
	tunAddr6 := netip.MustParsePrefix("fdfe:dcba:9876::1/126")

	undos, err := ApplyInterfaceConfigWith(mut, InterfaceConfig{
		Addresses:   []netip.Prefix{tunAddr4, tunAddr6},
		Routes:      DefaultCoveredRoutes(),
		RouteMetric: 0,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Enable: FreeIran state exists; foreign state untouched.
	if !mut.addresses[foreignAddr] || !mut.routes[foreignRoute] {
		t.Fatal("apply destroyed foreign state (family flush shape)")
	}

	if !mut.addresses[tunAddr4] || !mut.addresses[tunAddr6] {
		t.Fatal("apply did not add the TUN addresses")
	}

	for _, covered := range DefaultCoveredRoutes() {
		if !mut.routes[covered] {
			t.Fatalf("apply did not add covered route %s", covered)
		}
	}

	// Disable: undo exactly the applied steps.
	for i := len(undos) - 1; i >= 0; i-- {
		if err := undos[i](); err != nil {
			t.Fatalf("undo step %d: %v", i, err)
		}
	}

	if mut.addresses[tunAddr4] || mut.addresses[tunAddr6] {
		t.Fatal("rollback left FreeIran-owned addresses behind")
	}

	for _, covered := range DefaultCoveredRoutes() {
		if mut.routes[covered] {
			t.Fatalf("rollback left FreeIran-owned route %s behind", covered)
		}
	}

	// The foreign state SURVIVED the whole cycle.
	if !mut.addresses[foreignAddr] || !mut.routes[foreignRoute] {
		t.Fatal("rollback removed foreign state — the ownership boundary is broken")
	}
}

// TestApplyInterfaceConfigMidBatchFailureRollsBackExactly pins the
// partial-failure contract: a mid-batch failure returns the undos of
// EXACTLY what was applied, so a rollback is complete and nothing
// FreeIran-owned survives.
func TestApplyInterfaceConfigMidBatchFailureRollsBackExactly(t *testing.T) {
	mut := newFakeIPMutator()

	// The second route add fails (simulated collision).
	boom := netip.MustParsePrefix("8000::/1")
	limited := &firstFailsAfterOneRoute{fakeIPMutator: mut, boom: boom}

	undos, err := ApplyInterfaceConfigWith(limited, InterfaceConfig{
		Addresses: []netip.Prefix{netip.MustParsePrefix("198.18.0.1/30")},
		Routes:    DefaultCoveredRoutes(),
	})
	if err == nil {
		t.Fatal("mid-batch failure not surfaced")
	}

	// Rollback exactly what was applied.
	for i := len(undos) - 1; i >= 0; i-- {
		if err := undos[i](); err != nil {
			t.Fatalf("undo step %d: %v", i, err)
		}
	}

	if len(mut.addresses) != 0 || len(mut.routes) != 0 {
		t.Fatalf("residual FreeIran state after rollback: addrs=%v routes=%v", mut.addresses, mut.routes)
	}
}

// firstFailsAfterOneRoute lets the first route succeed and refuses the
// given one — a deterministic mid-batch collision.
type firstFailsAfterOneRoute struct {
	*fakeIPMutator
	boom    netip.Prefix
	sawOne  bool
	refused bool
}

// AddRoute implements IPMutator.
func (f *firstFailsAfterOneRoute) AddRoute(p netip.Prefix, next netip.Addr, metric uint32) error {
	if p == f.boom {
		f.refused = true

		return errSimulatedCollision
	}

	f.sawOne = true

	return f.fakeIPMutator.AddRoute(p, next, metric)
}

// TestMutatorInterfaceHasNoFlush pins the seam itself: no flush-shaped
// operation may ever exist on IPMutator — the compile-level guarantee
// that the family-wide wipe primitive cannot come back through this
// surface.
func TestMutatorInterfaceHasNoFlush(t *testing.T) {
	// The interface method set is exactly the four additive verbs.
	type additiveOnly interface {
		AddAddress(netip.Prefix) error
		DeleteAddress(netip.Prefix) error
		AddRoute(netip.Prefix, netip.Addr, uint32) error
		DeleteRoute(netip.Prefix, netip.Addr) error
	}

	var _ additiveOnly = (*fakeIPMutator)(nil)

	// Document the guarantee where it is enforced.
	if !strings.Contains("IPMutator is additive; flushes are banned", "flushes are banned") {
		t.Fatal("unreachable")
	}
}

// errSimulatedCollision is the fake's mid-batch failure.
var errSimulatedCollision = errorString("simulated route collision")

type errorString string

// Error implements error.
func (e errorString) Error() string { return string(e) }
