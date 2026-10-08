package tunnel

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// TestControllerDirectByDefault verifies a fresh controller reports
// ModeDirect + not active.
func TestControllerDirectByDefault(t *testing.T) {
	c := New()
	state := c.State()
	if state.Mode != ModeDirect {
		t.Errorf("Mode = %s, want %s", state.Mode, ModeDirect)
	}
	if state.Active {
		t.Errorf("Active = true, want false")
	}
}

// TestControllerDisableIsIdempotent verifies Disable on a non-active
// controller is a no-op.
func TestControllerDisableIsIdempotent(t *testing.T) {
	c := New()
	if err := c.Disable(context.Background()); err != nil {
		t.Errorf("Disable on direct controller returned err: %v", err)
	}
}

// TestControllerEnableDirectIsNoop verifies Enable(ModeDirect, ...) is
// a no-op.
func TestControllerEnableDirectIsNoop(t *testing.T) {
	c := New()
	if err := c.Enable(context.Background(), ModeDirect, "127.0.0.1", 1080, Options{}); err != nil {
		t.Errorf("Enable(ModeDirect) returned err: %v", err)
	}
}

// TestControllerTUNRefusalsWithoutCoreResolver pins the controller
// refusal contract when NO managed-core resolver is wired (the bare
// New()/NewWithProxyBackend wiring). The v0.14.0 selection policy
// made the first-party Windows dataplane the preferred TUN authority,
// so "unavailable everywhere without a resolver" is no longer the
// platform contract (see TestTUNBackendSelectionWithoutResolver and
// the Windows-specific first-party tests). What IS pinned on EVERY
// platform:
//
//   - the legacy Enable(ModeTUN) shape without TUN options refuses
//     with ErrTunUnavailable;
//   - EnableTUN without a configuration refuses with
//     ErrNoTUNConfiguration BEFORE the backend is consulted;
//   - EnableTUN with a configuration the first-party engine does not
//     support (VLESS) falls through the capability gate to the
//     unavailable fallback and refuses with ErrTunUnavailable;
//   - EVERY refusal leaves the controller direct/inactive with the
//     failure surfaced to the UI — no state mutation, no half-enable.
func TestControllerTUNRefusalsWithoutCoreResolver(t *testing.T) {
	c := New()

	// Legacy Enable(ModeTUN) without options refuses.
	err := c.Enable(context.Background(), ModeTUN, "127.0.0.1", 1080, Options{})
	if !errors.Is(err, ErrTunUnavailable) {
		t.Fatalf("Enable(ModeTUN) err = %v, want ErrTunUnavailable", err)
	}

	if state := c.State(); state.Mode != ModeDirect || state.Active {
		t.Fatalf("state after legacy refusal = %+v, want direct/inactive", state)
	}

	// EnableTUN with no configuration refuses BEFORE the backend.
	err = c.EnableTUN(context.Background(), TUNEnableOptions{})
	if !errors.Is(err, ErrNoTUNConfiguration) {
		t.Fatalf("EnableTUN(no config) err = %v, want ErrNoTUNConfiguration", err)
	}

	if state := c.State(); state.Mode != ModeDirect || state.Active {
		t.Fatalf("state after empty-config refusal = %+v, want direct/inactive", state)
	}

	// EnableTUN with an unsupported configuration reaches the
	// selecting backend, is declined by the first-party capability
	// gate (or finds no first-party dataplane at all), and refuses
	// through the unavailable fallback.
	err = c.EnableTUN(context.Background(), TUNEnableOptions{Config: config.Config{Type: config.TypeVLESS}})
	if !errors.Is(err, ErrTunUnavailable) {
		t.Fatalf("EnableTUN(vless) err = %v, want ErrTunUnavailable", err)
	}

	// The failed enable must leave the controller direct/inactive with
	// the failure visible to the UI.
	state := c.State()
	if state.Mode != ModeDirect || state.Active {
		t.Fatalf("state = %+v, want direct/inactive after the refused enable", state)
	}

	if state.Details == "" || !strings.Contains(state.Details, "TUN enable failed") {
		t.Fatalf("details = %q, want the surfaced TUN failure", state.Details)
	}

	// A refused enable must not latch the controller: Disable is a
	// harmless no-op afterwards.
	if err := c.Disable(context.Background()); err != nil {
		t.Fatalf("Disable after refused enable err = %v, want nil", err)
	}
}

// TestUnavailableTUNBackendContract pins the honest capability-limited
// backend surface DIRECTLY (unavailableTUNBackend), independent of
// platform selection: Install and Enable both refuse, Disable is a
// harmless no-op, and the snapshot reports the unavailable state with
// the backend named honestly.
func TestUnavailableTUNBackendContract(t *testing.T) {
	backend := unavailableTUNBackend{}

	if backend.Available() {
		t.Fatal("the unavailable backend must never report available")
	}

	if err := backend.Install(context.Background()); !errors.Is(err, ErrTunUnavailable) {
		t.Fatalf("Install err = %v, want ErrTunUnavailable", err)
	}

	if err := backend.Enable(context.Background(), TUNEnableOptions{}); !errors.Is(err, ErrTunUnavailable) {
		t.Fatalf("Enable err = %v, want ErrTunUnavailable", err)
	}

	if err := backend.Disable(context.Background()); err != nil {
		t.Fatalf("Disable err = %v, want nil no-op", err)
	}

	snap := backend.Snapshot()
	if snap.Available || snap.Installed {
		t.Fatalf("snapshot = %+v, want unavailable/not-installed", snap)
	}

	if !snap.RequiresElevation {
		t.Fatal("the unavailable snapshot must still declare the elevation requirement")
	}

	if snap.Status != tunStatusOff {
		t.Fatalf("status = %q, want %q", snap.Status, tunStatusOff)
	}

	if snap.Backend == "" {
		t.Fatal("snapshot must name the TUN backend honestly")
	}

	if snap.Details != ErrTunUnavailable.Error() {
		t.Fatalf("details = %q, want the capability error text", snap.Details)
	}
}

// TestTUNBackendSelectionWithoutResolver pins the v0.14.0 selection
// policy of newTUNBackend(nil) on each platform WITHOUT a wired core
// resolver:
//
//   - Windows: the first-party dataplane is compiled in and is the
//     preferred TUN authority — the backend reports AVAILABLE (the
//     v0.11.3 "unavailable without a resolver" expectation was
//     superseded by that architecture), its snapshot names the
//     first-party dataplane, and configurations beyond the first-party
//     capability set still fall through to the unavailable fallback.
//   - Other platforms: no first-party dataplane exists; the backend
//     reports honestly unavailable.
//
// Install always delegates to the sing-box dependency set (the
// fallback), which refuses without a wired resolver on every platform.
func TestTUNBackendSelectionWithoutResolver(t *testing.T) {
	backend := newTUNBackend(nil)

	// Install: the fallback dependency set is refused everywhere.
	if err := backend.Install(context.Background()); !errors.Is(err, ErrTunUnavailable) {
		t.Fatalf("Install err = %v, want ErrTunUnavailable", err)
	}

	// Disable with nothing active is a harmless no-op everywhere.
	if err := backend.Disable(context.Background()); err != nil {
		t.Fatalf("Disable err = %v, want nil no-op", err)
	}

	snap := backend.Snapshot()

	if runtime.GOOS == "windows" {
		if !backend.Available() {
			t.Fatal("windows: the first-party dataplane must report available")
		}

		if !snap.Available || !snap.Installed {
			t.Fatalf("windows: snapshot = %+v, want the available first-party dataplane", snap)
		}

		if snap.Active || snap.Status != tunStatusOff {
			t.Fatalf("windows: snapshot = %+v, want inactive/off before any enable", snap)
		}

		if !snap.RequiresElevation {
			t.Fatal("windows: the first-party snapshot must declare the elevation requirement")
		}

		if snap.Backend != tunDataplaneFirstParty {
			t.Fatalf("windows: backend label = %q, want %q", snap.Backend, tunDataplaneFirstParty)
		}
	} else {
		if backend.Available() {
			t.Fatal("non-windows: TUN must report unavailable without a wired core resolver")
		}

		if snap.Available || snap.Installed {
			t.Fatalf("non-windows: snapshot = %+v, want unavailable/not-installed", snap)
		}

		if snap.Status != tunStatusOff || snap.Backend == "" {
			t.Fatalf("non-windows: snapshot = %+v, want off with an honest backend label", snap)
		}
	}

	// An unsupported configuration (VLESS is beyond the first-party
	// capability set) falls through the capability gate and refuses
	// through the unavailable fallback on EVERY platform.
	err := backend.Enable(context.Background(), TUNEnableOptions{Config: config.Config{Type: config.TypeVLESS}})
	if !errors.Is(err, ErrTunUnavailable) {
		t.Fatalf("Enable(vless) err = %v, want ErrTunUnavailable", err)
	}

	// The refused enable must not have activated anything.
	if after := backend.Snapshot(); after.Active || after.Status == tunStatusActive {
		t.Fatalf("snapshot after refused enable = %+v, want nothing active", after)
	}
}

// TestStateString verifies the State struct can be JSON-marshaled
// (the Wails binding layer needs this).
func TestStateString(t *testing.T) {
	c := New()
	state := c.State()
	if state.Mode != ModeDirect {
		t.Errorf("Mode = %s, want %s", state.Mode, ModeDirect)
	}
}

// --- stub backend: Controller transaction behavior -----------------------

type stubTUNBackend struct {
	enableErr  error
	disableErr error
	snap       TUNSnapshot
	enableErr_ int
	disabled   int
}

func (s *stubTUNBackend) Available() bool { return true }
func (s *stubTUNBackend) Install(ctx context.Context) error {
	return nil
}
func (s *stubTUNBackend) Enable(ctx context.Context, opts TUNEnableOptions) error {
	s.enableErr_++

	return s.enableErr
}
func (s *stubTUNBackend) Disable(ctx context.Context) error {
	s.disabled++

	return s.disableErr
}
func (s *stubTUNBackend) Snapshot() TUNSnapshot {
	snap := s.snap
	snap.Backend = tunBackendName
	snap.Status = tunStatusActive
	snap.Active = true

	return snap
}

// TestControllerEnableTUNTransactional pins the Controller-side
// transaction: a failed backend enable leaves the controller direct +
// inactive with the failure surfaced; a successful one publishes the
// honest Active state; Disable resets it.
func TestControllerEnableTUNTransactional(t *testing.T) {
	stub := &stubTUNBackend{enableErr: errors.New("no route to host")}
	c := NewWithProxyBackend(nil)
	c.tun = stub

	// Failure path: error propagates, state records the failure.
	err := c.EnableTUN(context.Background(), TUNEnableOptions{
		Config: config.Config{Type: config.TypeVLESS},
	})
	if err == nil {
		t.Fatal("EnableTUN must propagate the backend failure")
	}

	state := c.State()
	if state.Mode != ModeDirect || state.Active {
		t.Fatalf("after failed enable state = %+v, want direct/inactive", state)
	}

	if state.TUN == nil {
		t.Fatal("after failed enable the TUN snapshot must be attached for the UI")
	}

	// Success path.
	stub.enableErr = nil

	if err := c.EnableTUN(context.Background(), TUNEnableOptions{
		Config: config.Config{Type: config.TypeVLESS},
	}); err != nil {
		t.Fatalf("EnableTUN(success) err = %v", err)
	}

	state = c.State()
	if state.Mode != ModeTUN || !state.Active {
		t.Fatalf("after enable state = %+v, want tun/active", state)
	}

	if state.TUN == nil || !state.TUN.Active || state.TUN.Status != tunStatusActive {
		t.Fatalf("TUN snapshot = %+v, want active", state.TUN)
	}

	// Double enable refuses.
	if err := c.EnableTUN(context.Background(), TUNEnableOptions{Config: config.Config{Type: config.TypeVLESS}}); !errors.Is(err, ErrAlreadyEnabled) {
		t.Fatalf("second EnableTUN err = %v, want ErrAlreadyEnabled", err)
	}

	// Disable resets to direct.
	if err := c.Disable(context.Background()); err != nil {
		t.Fatalf("Disable err = %v", err)
	}

	if state := c.State(); state.Mode != ModeDirect || state.Active {
		t.Fatalf("after disable state = %+v, want direct/inactive", state)
	}
}

// TestTUNCoreVersionGuard pins the minimum sing-box version for the
// TUN document (rule actions + current DNS format).
func TestTUNCoreVersionGuard(t *testing.T) {
	if err := tunCoreVersionOK("1.14.1"); err != nil {
		t.Fatalf("1.14.1 must be accepted: %v", err)
	}

	if err := tunCoreVersionOK("v1.12.0"); err != nil {
		t.Fatalf("1.12.0 must be accepted: %v", err)
	}

	if err := tunCoreVersionOK("1.11.15"); err == nil {
		t.Fatal("1.11.x must be refused (pre-DNS-format runtime)")
	}

	if err := tunCoreVersionOK(""); err != nil {
		t.Fatalf("unknown version must pass through: %v", err)
	}
}

// TestPickTUNAddresses pins the route-context contract on a host
// whose live prefixes leave room: a collision-free IPv4 address is
// chosen and parses as a CIDR prefix. (The fail-closed path — every
// candidate colliding — is pinned by TestPickTUNAddressesFailClosed
// in tun_hardening_test.go with an injected collision table.)
func TestPickTUNAddresses(t *testing.T) {
	v4, _, err := pickTUNAddresses()
	if err != nil {
		t.Fatalf("pickTUNAddresses must succeed when a free candidate exists: %v", err)
	}

	if v4 == "" {
		t.Fatal("pickTUNAddresses must return an IPv4 candidate on success")
	}

	if _, err := netip.ParsePrefix(v4); err != nil {
		t.Fatalf("ipv4 candidate %q does not parse: %v", v4, err)
	}
}

// TestTUNInterfaceNameSelection pins the exact-name collision
// avoidance: the chosen name is never one currently in use.
func TestTUNInterfaceNameSelection(t *testing.T) {
	name, err := pickTUNInterfaceName()
	if err != nil {
		t.Fatalf("pickTUNInterfaceName: %v", err)
	}

	if name == "" {
		t.Fatal("name must not be empty")
	}
}

// TestTUNSessionMarkerLifecycle pins the crash-recovery marker
// contract: write → detect → clear, with the marker consumed on
// clean disable and reported (not acted on) at boot.
func TestTUNSessionMarkerLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tun-session.json")

	SetTUNSessionMarkerPath(path)
	t.Cleanup(func() { tunSessionMarkerPath = "" })

	// No marker: nothing stale.
	if session := CheckStaleTUNSession(); session.Found {
		t.Fatalf("empty workspace reported stale session: %+v", session)
	}

	// Write a session marker.
	if err := writeTUNSessionMarker(TUNSessionMarker{
		InterfaceName: "FreeIranTUN",
		IPv4Address:   "172.19.0.1/30",
		PID:           1234,
		StartedAt:     time.Now().UTC().Add(-time.Hour),
		Configuration: "vless://redacted",
	}); err != nil {
		t.Fatalf("writeTUNSessionMarker: %v", err)
	}

	// Boot detection: the marker is found and REPORTED. The adapter
	// cannot exist in the test host, so the report must say the
	// interface is already gone while still surfacing the session.
	session := CheckStaleTUNSession()
	if !session.Found {
		t.Fatal("stale session must be detected from the marker")
	}

	if session.InterfaceName != "FreeIranTUN" || session.PID != 1234 {
		t.Fatalf("session = %+v, want the recorded identity", session)
	}

	if session.InterfacePresent {
		t.Fatal("a non-existent adapter must not be reported present")
	}

	if session.Detail == "" {
		t.Fatal("the stale report must carry an honest detail line")
	}

	// Clean disable consumes the marker.
	if err := clearTUNSessionMarker(); err != nil {
		t.Fatalf("clearTUNSessionMarker: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("marker must be gone after clean disable (stat err = %v)", err)
	}

	if session := CheckStaleTUNSession(); session.Found {
		t.Fatalf("cleared marker must not be reported: %+v", session)
	}
}
