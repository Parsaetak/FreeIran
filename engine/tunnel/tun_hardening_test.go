package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// tun_hardening_test.go pins the v0.11.4 TUN correctness repairs
// with deterministic seams (no live network mutation, no elevated
// requirement, runnable on every platform):
//
//   - pickTUNAddresses fails CLOSED when every IPv4 candidate
//     collides with a live prefix (the v0.11.3 silent fallback is
//     gone);
//   - findTUNInterface accepts ONLY the exact session adapter
//     (recorded name AND expected address on the SAME interface) —
//     the wrong-adapter/same-address false positive is rejected;
//   - the route-observation verdicts return the correct
//     ownership/path facts for default-route, /1-pair, split and
//     absent coverage;
//   - the upstream-pinning verdicts detect the loop (socket sourced
//     from the TUN address) and the positive physical-side pinning;
//   - verifyTUNActivation requires EVERY evidence class before
//     success — each missing class fails with its precise error;
//   - rollbackEnable clears the owned session state and surfaces a
//     residual adapter instead of silencing it.

// --- helpers --------------------------------------------------------------

// withTUNSeams installs deterministic fakes for every observation
// seam and restores them when the test ends.
func withTUNSeams(t *testing.T, ifaces func() ([]tunInterfaceSnapshot, error), prefixes func() []netip.Prefix, routes func() ([]observedRoute, error), sockets func() ([]observedOwnerSocket, error), probe func(context.Context) error) {
	t.Helper()

	oldIfaces, oldPrefixes := tunInterfaceSnapshots, tunExistingLocalPrefixes
	oldRoutes, oldSockets, oldProbe := collectForwardRoutes, collectOwnerTCPSockets, tunProbeInternet

	tunInterfaceSnapshots = ifaces
	tunExistingLocalPrefixes = prefixes
	collectForwardRoutes = routes
	collectOwnerTCPSockets = sockets
	tunProbeInternet = probe

	t.Cleanup(func() {
		tunInterfaceSnapshots = oldIfaces
		tunExistingLocalPrefixes = oldPrefixes
		collectForwardRoutes = oldRoutes
		collectOwnerTCPSockets = oldSockets
		tunProbeInternet = oldProbe
	})
}

func staticIfaces(snaps ...tunInterfaceSnapshot) func() ([]tunInterfaceSnapshot, error) {
	return func() ([]tunInterfaceSnapshot, error) { return snaps, nil }
}

func staticPrefixes(ps ...netip.Prefix) func() []netip.Prefix {
	return func() []netip.Prefix { return ps }
}

func staticRoutes(rs ...observedRoute) func() ([]observedRoute, error) {
	return func() ([]observedRoute, error) { return rs, nil }
}

func staticSockets(ss ...observedOwnerSocket) func() ([]observedOwnerSocket, error) {
	return func() ([]observedOwnerSocket, error) { return ss, nil }
}

func errSeams() (func() ([]observedRoute, error), func() ([]observedOwnerSocket, error)) {
	return func() ([]observedRoute, error) { return nil, ErrTUNRouteObservationUnavailable },
		func() ([]observedOwnerSocket, error) { return nil, ErrTUNRouteObservationUnavailable }
}

// allIPv4CandidatesAsPrefixes builds the collision table that covers
// every IPv4 TUN candidate (172.19.0.0/30 pair blocks .1 and .5 and
// .9 via a wider 172.16.0.0/12-style supernet — simpler: cover each
// candidate directly).
func allIPv4CandidatesAsPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(tunIPv4Candidates))

	for _, c := range tunIPv4Candidates {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			panic(err)
		}

		out = append(out, p)
	}

	return out
}

// shortTUNWaits shortens the activation/removal poll deadlines for
// failure-path tests (the production 20s/10s budgets would make the
// negative cases crawl).
func shortTUNWaits(t *testing.T) {
	t.Helper()

	oldWait, oldRemove := tunInterfaceWaitTimeout, tunInterfaceRemovalWaitTimeout
	tunInterfaceWaitTimeout = 400 * time.Millisecond
	tunInterfaceRemovalWaitTimeout = 400 * time.Millisecond

	t.Cleanup(func() {
		tunInterfaceWaitTimeout = oldWait
		tunInterfaceRemovalWaitTimeout = oldRemove
	})
}

// --- 3. fail-closed address selection -------------------------------------

// TestPickTUNAddressesFailClosed pins the v0.11.4 safety contract:
// when EVERY IPv4 candidate overlaps a live prefix the selection
// returns an explicit error — never the v0.11.3 silent fallback onto
// a colliding subnet.
func TestPickTUNAddressesFailClosed(t *testing.T) {
	withTUNSeams(t,
		staticIfaces(),
		staticPrefixes(allIPv4CandidatesAsPrefixes()...),
		nil, nil, nil,
	)

	v4, v6, err := pickTUNAddresses()
	if err == nil {
		t.Fatalf("all candidates colliding must FAIL CLOSED (got %q/%q)", v4, v6)
	}

	if v4 != "" || v6 != "" {
		t.Fatalf("a failed selection must return no addresses (got %q/%q)", v4, v6)
	}

	if !strings.Contains(err.Error(), "collision-free") {
		t.Fatalf("the failure must explain the collision: %v", err)
	}
}

// TestPickTUNAddressesChoosesFreeCandidate verifies partial
// collisions select the first FREE candidate (not the colliding
// default).
func TestPickTUNAddressesChoosesFreeCandidate(t *testing.T) {
	// Cover only the documentation default 172.19.0.1/30.
	first, err := netip.ParsePrefix(tunIPv4Candidates[0])
	if err != nil {
		t.Fatal(err)
	}

	withTUNSeams(t, staticIfaces(), staticPrefixes(first), nil, nil, nil)

	v4, _, err := pickTUNAddresses()
	if err != nil {
		t.Fatalf("a free candidate exists — selection must succeed: %v", err)
	}

	if v4 != tunIPv4Candidates[1] {
		t.Fatalf("expected the second candidate %q (the first collides), got %q", tunIPv4Candidates[1], v4)
	}
}

// --- 4+5. exact adapter identity -------------------------------------------

// TestFindTUNInterfaceExactIdentity pins the activation-observation
// invariant: ONLY the interface with the recorded FreeIran adapter
// name AND the expected session address (on that same interface) is
// accepted. The v0.11.3 false positives — same address on a
// DIFFERENT adapter, or the right name WITHOUT the address — are
// both rejected.
func TestFindTUNInterfaceExactIdentity(t *testing.T) {
	const (
		wantName = "FreeIranTUN"
		wantAddr = "172.19.0.1"
	)

	cases := []struct {
		name   string
		snap   []tunInterfaceSnapshot
		accept bool
		reason string
	}{
		{
			name: "exact adapter with expected address",
			snap: []tunInterfaceSnapshot{
				{Name: "Ethernet", Index: 12, Addrs: []string{"192.168.1.5/24"}},
				{Name: wantName, Index: 42, Addrs: []string{wantAddr + "/30"}},
			},
			accept: true,
		},
		{
			name: "wrong adapter carrying the expected address (the v0.11.3 false positive)",
			snap: []tunInterfaceSnapshot{
				{Name: "Ethernet", Index: 12, Addrs: []string{wantAddr + "/30"}},
				{Name: wantName, Index: 42, Addrs: []string{"10.1.2.3/24"}},
			},
			accept: false,
		},
		{
			name:   "right name without the expected address",
			snap:   []tunInterfaceSnapshot{{Name: wantName, Index: 42, Addrs: []string{"10.0.0.9/30"}}},
			accept: false,
		},
		{
			name:   "right name with no addresses at all",
			snap:   []tunInterfaceSnapshot{{Name: wantName, Index: 42}},
			accept: false,
		},
		{
			name:   "adapter absent entirely",
			snap:   []tunInterfaceSnapshot{{Name: "Ethernet", Index: 12, Addrs: []string{wantAddr + "/30"}}},
			accept: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTUNSeams(t, staticIfaces(tc.snap...), nil, nil, nil, nil)

			snap, ok := findTUNInterface(wantName, wantAddr+"/30")
			if ok != tc.accept {
				t.Fatalf("findTUNInterface accept = %v, want %v", ok, tc.accept)
			}

			if tc.accept && snap.Index != 42 {
				t.Fatalf("accepted snapshot must carry the session adapter identity (index %d, want 42)", snap.Index)
			}
		})
	}
}

// TestTUNInterfaceNamed pins the teardown-presence check: any
// adapter carrying the FreeIran-owned name counts as present (the
// collision-free name makes name presence the honest residual
// signal), independent of the session address.
func TestTUNInterfaceNamed(t *testing.T) {
	withTUNSeams(t,
		staticIfaces(
			tunInterfaceSnapshot{Name: "Ethernet", Index: 12, Addrs: []string{"192.168.1.5/24"}},
			tunInterfaceSnapshot{Name: "FreeIranTUN", Index: 42, Addrs: []string{"172.19.0.1/30"}},
		), nil, nil, nil, nil,
	)

	if !tunInterfaceNamed("FreeIranTUN") {
		t.Fatal("the named adapter is present — must be reported")
	}

	if tunInterfaceNamed("FreeIranTUN9") {
		t.Fatal("an absent name must not be reported")
	}

	if tunInterfaceNamed("") {
		t.Fatal("the empty name must never be reported")
	}
}

// --- 6. route observation verdicts -----------------------------------------

func TestTUNRouteCoveringVerdict(t *testing.T) {
	const tunIfIndex = uint32(42)

	cases := []struct {
		name    string
		routes  []observedRoute
		covered bool
	}{
		{
			name:    "default route on the TUN",
			routes:  []observedRoute{{IfIndex: tunIfIndex}},
			covered: true,
		},
		{
			name: "covering /1 pair on the TUN",
			routes: []observedRoute{
				{Mask: [4]byte{128, 0, 0, 0}, IfIndex: tunIfIndex},
				{Dest: [4]byte{128, 0, 0, 0}, Mask: [4]byte{128, 0, 0, 0}, IfIndex: tunIfIndex},
			},
			covered: true,
		},
		{
			name: "default route on ANOTHER interface (traffic would bypass the TUN)",
			routes: []observedRoute{
				{IfIndex: 12},
				{Mask: [4]byte{255, 255, 255, 252}, IfIndex: tunIfIndex},
			},
			covered: false,
		},
		{
			name: "only the lower /1 half on the TUN",
			routes: []observedRoute{
				{Mask: [4]byte{128, 0, 0, 0}, IfIndex: tunIfIndex},
				{Dest: [4]byte{128, 0, 0, 0}, Mask: [4]byte{128, 0, 0, 0}, IfIndex: 12},
			},
			covered: false,
		},
		{
			name:    "no routes at all",
			routes:  nil,
			covered: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := tunRouteCoveringVerdict(tc.routes, tunIfIndex)
			if verdict.Covered != tc.covered {
				t.Fatalf("covered = %v, want %v (detail: %s)", verdict.Covered, tc.covered, verdict.Detail)
			}

			if verdict.Detail == "" {
				t.Fatal("the verdict must always carry a bounded explanation")
			}
		})
	}

	// The zero interface index must never be "covered".
	if verdict := tunRouteCoveringVerdict([]observedRoute{{IfIndex: 0}}, 0); verdict.Covered {
		t.Fatal("a zero TUN interface index must not be covered")
	}
}

func TestTUNUpstreamPinningVerdict(t *testing.T) {
	var (
		corePID  = uint32(4242)
		tunAddr  = [4]byte{172, 19, 0, 1}
		physAddr = [4]byte{192, 168, 1, 5}
	)

	t.Run("loop: sing-box socket sourced from the TUN address", func(t *testing.T) {
		v := tunUpstreamPinningVerdict(
			[]observedOwnerSocket{
				{PID: corePID, LocalAddr: tunAddr, LocalPort: 52000},
			}, corePID, tunAddr)

		if !v.Loop {
			t.Fatal("a sing-box socket sourced from the TUN address IS the route loop")
		}

		if v.Pinned {
			t.Fatal("a loop is not pinning evidence")
		}
	})

	t.Run("pinned: sing-box upstream sourced from the physical side", func(t *testing.T) {
		v := tunUpstreamPinningVerdict(
			[]observedOwnerSocket{
				{PID: corePID, LocalAddr: physAddr, LocalPort: 51000},
			}, corePID, tunAddr)

		if v.Loop {
			t.Fatal("a physical-side socket is not a loop")
		}

		if !v.Pinned {
			t.Fatal("a physical-side sing-box socket IS positive pinning evidence")
		}
	})

	t.Run("no sing-box sockets (UDP-family outbound)", func(t *testing.T) {
		v := tunUpstreamPinningVerdict(
			[]observedOwnerSocket{
				{PID: 7, LocalAddr: physAddr, LocalPort: 9},
			}, corePID, tunAddr)

		if v.Loop || v.Pinned {
			t.Fatalf("other processes' sockets are neither loop nor pinning evidence: %+v", v)
		}

		if !strings.Contains(v.Detail, "not observable") {
			t.Fatalf("the honest not-observable detail must be present: %s", v.Detail)
		}
	})

	t.Run("zero PID refuses to reason", func(t *testing.T) {
		v := tunUpstreamPinningVerdict(nil, 0, tunAddr)
		if v.Loop || v.Pinned {
			t.Fatalf("no PID — no verdict: %+v", v)
		}
	})

	t.Run("wildcard and loopback sockets are not pinning evidence", func(t *testing.T) {
		v := tunUpstreamPinningVerdict(
			[]observedOwnerSocket{
				{PID: corePID, LocalAddr: [4]byte{}, LocalPort: 1},
				{PID: corePID, LocalAddr: [4]byte{127, 0, 0, 1}, LocalPort: 2},
			}, corePID, tunAddr)

		if v.Pinned {
			t.Fatal("wildcard/loopback sourcing is not physical-side pinning evidence")
		}
	})
}

// --- 7. the activation gate requires every evidence class ------------------

// TestVerifyTUNActivationEvidenceMatrix runs the full activation
// proof chain against controlled seams: every required evidence
// class must be present, and each missing class fails with its
// precise error.
func TestVerifyTUNActivationEvidenceMatrix(t *testing.T) {
	shortTUNWaits(t)

	const (
		name    = "FreeIranTUN"
		cidr    = "172.19.0.1/30"
		corePID = 4242
	)

	goodIfaces := staticIfaces(
		tunInterfaceSnapshot{Name: "Ethernet", Index: 12, Addrs: []string{"192.168.1.5/24"}},
		tunInterfaceSnapshot{Name: name, Index: 42, Addrs: []string{cidr}},
	)

	badIfaces := staticIfaces(
		// The expected address exists — but on the WRONG adapter.
		tunInterfaceSnapshot{Name: "Ethernet", Index: 12, Addrs: []string{cidr}},
		tunInterfaceSnapshot{Name: name, Index: 42, Addrs: []string{"10.0.0.9/30"}},
	)

	goodRoutes := staticRoutes(observedRoute{IfIndex: 42}) // default route on the TUN
	badRoutes := staticRoutes(observedRoute{IfIndex: 12})  // default route elsewhere

	goodSockets := staticSockets(observedOwnerSocket{PID: corePID, LocalAddr: [4]byte{192, 168, 1, 5}, LocalPort: 51000})
	loopSockets := staticSockets(observedOwnerSocket{PID: corePID, LocalAddr: [4]byte{172, 19, 0, 1}, LocalPort: 52000})

	probeOK := func(context.Context) error { return nil }
	probeFail := func(context.Context) error { return errors.New("dial timeout") }

	errRoutes, errSockets := errSeams()

	cases := []struct {
		name    string
		ifaces  func() ([]tunInterfaceSnapshot, error)
		routes  func() ([]observedRoute, error)
		sockets func() ([]observedOwnerSocket, error)
		probe   func(context.Context) error
		wantErr string
	}{
		{name: "all evidence present", ifaces: goodIfaces, routes: goodRoutes, sockets: goodSockets, probe: probeOK},
		{name: "wrong adapter with the session address", ifaces: badIfaces, routes: goodRoutes, sockets: goodSockets, probe: probeOK, wantErr: "was not observed"},
		{name: "route not owned by the TUN", ifaces: goodIfaces, routes: badRoutes, sockets: goodSockets, probe: probeOK, wantErr: "does not own the covering IPv4 route state"},
		{name: "route observation unavailable fails closed", ifaces: goodIfaces, routes: errRoutes, sockets: goodSockets, probe: probeOK, wantErr: "route observation failed"},
		{name: "traffic probe fails", ifaces: goodIfaces, routes: goodRoutes, sockets: goodSockets, probe: probeFail, wantErr: "tunneled Internet request failed"},
		{name: "upstream loop", ifaces: goodIfaces, routes: goodRoutes, sockets: loopSockets, probe: probeOK, wantErr: "route-loop"},
		{name: "socket observation unavailable fails closed", ifaces: goodIfaces, routes: goodRoutes, sockets: errSockets, probe: probeOK, wantErr: "upstream socket observation failed"},
		{name: "UDP-family outbound (no TCP sockets) still activates", ifaces: goodIfaces, routes: goodRoutes, sockets: staticSockets(), probe: probeOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTUNSeams(t, tc.ifaces, nil, tc.routes, tc.sockets, tc.probe)

			err := verifyTUNActivation(context.Background(), name, cidr, corePID)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected activation with complete evidence, got: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected failure containing %q, got success", tc.wantErr)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q must contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// --- 8+9. rollback clears owned state and surfaces residuals ----------------

// newRollbackBackend builds a singboxTUNBackend carrying ONLY the
// recorded session state (no live instance — rollback tolerates a
// nil instance by design; the launch-failure path has none).
func newRollbackBackend(name string) *singboxTUNBackend {
	b := &singboxTUNBackend{status: tunStatusStarting}
	b.settings.InterfaceName = name
	b.settings.IPv4Address = "172.19.0.1/30"
	return b
}

// TestRollbackEnableClearsSessionState pins the transactional
// rollback contract: after a failed activation the owned session
// state is cleared and the cause is surfaced (composed, never
// swallowed).
func TestRollbackEnableClearsSessionState(t *testing.T) {
	shortTUNWaits(t)

	withTUNSeams(t, staticIfaces(tunInterfaceSnapshot{Name: "Ethernet", Index: 12}), nil, nil, nil, nil)

	b := newRollbackBackend("FreeIranTUN")
	cause := errors.New("route evidence failed")

	err := b.rollbackEnable(context.Background(), cause)
	if err == nil {
		t.Fatal("rollback must surface the cause")
	}

	if !errors.Is(err, cause) {
		t.Fatalf("rollback error must wrap the cause: %v", err)
	}

	b.mu.Lock()
	stateCleared := b.instance == nil && b.sessionCancel == nil && b.settings.InterfaceName == ""
	b.mu.Unlock()

	if !stateCleared {
		t.Fatal("rollback must clear the owned session state (instance, cancel, settings)")
	}

	if b.status != tunStatusFailed {
		t.Fatalf("status after rollback = %q, want %q", b.status, tunStatusFailed)
	}
}

// TestRollbackEnableSurfacesResidual pins the honest-residual
// contract: when the FreeIran-owned adapter is STILL present after
// teardown the rollback reports the residual (error + failed state)
// instead of claiming cleanup succeeded.
func TestRollbackEnableSurfacesResidual(t *testing.T) {
	shortTUNWaits(t)

	withTUNSeams(t,
		staticIfaces(tunInterfaceSnapshot{Name: "FreeIranTUN", Index: 42, Addrs: []string{"172.19.0.1/30"}}),
		nil, nil, nil, nil,
	)

	b := newRollbackBackend("FreeIranTUN")

	err := b.rollbackEnable(context.Background(), errors.New("route evidence failed"))
	if err == nil {
		t.Fatal("rollback must surface the cause")
	}

	if !strings.Contains(err.Error(), "RESIDUAL") {
		t.Fatalf("a leftover adapter must be surfaced as RESIDUAL: %v", err)
	}

	if b.status != tunStatusFailed {
		t.Fatalf("a residual rollback must leave status=failed, got %q", b.status)
	}
}

// TestWaitForTUNInterfaceRemovedBounded pins the bounded-polling
// contract: an adapter that disappears mid-poll ends the wait early.
func TestWaitForTUNInterfaceRemovedBounded(t *testing.T) {
	shortTUNWaits(t)

	var present atomic.Bool
	present.Store(true)

	withTUNSeams(t,
		func() ([]tunInterfaceSnapshot, error) {
			if present.Load() {
				return []tunInterfaceSnapshot{{Name: "FreeIranTUN", Index: 42}}, nil
			}

			return nil, nil
		}, nil, nil, nil, nil,
	)

	go func() {
		time.Sleep(150 * time.Millisecond)
		present.Store(false)
	}()

	start := time.Now()

	if residual := waitForTUNInterfaceRemoved(context.Background(), "FreeIranTUN", 5*time.Second); residual {
		t.Fatal("the adapter disappeared — no residual must be reported")
	}

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("removal detection must be prompt, took %v", elapsed)
	}
}

// TestStaleTUNSessionDetailPrecision pins the crash-recovery report
// precision: a marker whose adapter exists WITHOUT the recorded
// address reports the refined detail, and the strict match reports
// the with-address detail.
func TestStaleTUNSessionDetailPrecision(t *testing.T) {
	dir := t.TempDir()
	path := fmt.Sprintf("%s/tun-session.json", dir)

	SetTUNSessionMarkerPath(path)
	t.Cleanup(func() { tunSessionMarkerPath = "" })

	if err := writeTUNSessionMarker(TUNSessionMarker{
		InterfaceName: "FreeIranTUN",
		IPv4Address:   "172.19.0.1/30",
		PID:           4242,
	}); err != nil {
		t.Fatalf("writeTUNSessionMarker: %v", err)
	}

	t.Run("adapter with the recorded address", func(t *testing.T) {
		withTUNSeams(t, staticIfaces(tunInterfaceSnapshot{Name: "FreeIranTUN", Index: 42, Addrs: []string{"172.19.0.1/30"}}), nil, nil, nil, nil)

		session := CheckStaleTUNSession()
		if !session.Found || !session.InterfacePresent {
			t.Fatalf("session = %+v, want found+present", session)
		}

		if !strings.Contains(session.Detail, "with its session address") {
			t.Fatalf("detail must report the with-address form: %s", session.Detail)
		}
	})

	t.Run("adapter without the recorded address", func(t *testing.T) {
		withTUNSeams(t, staticIfaces(tunInterfaceSnapshot{Name: "FreeIranTUN", Index: 42, Addrs: []string{"10.0.0.9/30"}}), nil, nil, nil, nil)

		session := CheckStaleTUNSession()
		if !session.Found || !session.InterfacePresent {
			t.Fatalf("session = %+v, want found+present (name-based residual)", session)
		}

		if !strings.Contains(session.Detail, "without the recorded session address") {
			t.Fatalf("detail must report the refined form: %s", session.Detail)
		}
	})

	t.Run("adapter gone", func(t *testing.T) {
		withTUNSeams(t, staticIfaces(tunInterfaceSnapshot{Name: "Ethernet", Index: 12}), nil, nil, nil, nil)

		session := CheckStaleTUNSession()
		if !session.Found || session.InterfacePresent {
			t.Fatalf("session = %+v, want found+not-present", session)
		}
	})
}

// TestControllerEnableTUNRefusesBeforeMutation pins the compatibility
// ordering contract at the controller seam: an EnableTUN call with an
// EMPTY configuration is refused with ErrNoTUNConfiguration BEFORE
// the backend is ever consulted (the app layer performs the full
// sing-box compatibility validation before this seam — see
// TunnelService.EnableTUN).
func TestControllerEnableTUNRefusesBeforeMutation(t *testing.T) {
	stub := &stubTUNBackend{}
	c := NewWithProxyBackend(nil)
	c.tun = stub

	err := c.EnableTUN(context.Background(), TUNEnableOptions{Config: config.Config{}})
	if !errors.Is(err, ErrNoTUNConfiguration) {
		t.Fatalf("EnableTUN(no config) err = %v, want ErrNoTUNConfiguration", err)
	}

	if stub.enableErr_ != 0 {
		t.Fatal("the refusal must happen BEFORE the backend is consulted")
	}
}
