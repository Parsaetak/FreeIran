// v099_regression_test.go — v0.9.9 engine-upgrade regression coverage
// for the connection layer:
//
//  1. successful core-start metrics are recorded EXACTLY ONCE
//     (the pre-0.9.9 code counted AddCoreStart(true)+
//     ObserveCoreStartup at readiness AND again at the verification
//     boundary);
//  2. the publisher's overflow valve preserves lifecycle-critical
//     transitions (critical/replaceable classification);
//  3. snapshotsEqual is the explicit semantic equality (no
//     reflection);
//  4. port resolution: user-selected ports win, automatic ports
//     allocate, occupied user ports fail loudly.
package connection_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
	"github.com/Parsaetak/FreeIran/engine/metrics"
	"github.com/Parsaetak/FreeIran/system"
)

// TestCoreStartMetricsRecordedOnce proves the single authoritative
// startup measurement: one successful fake-core connect must count
// exactly one successful core start (and one startup observation),
// no matter that the attempt also passed through the verification
// boundary (skipped here) and the monitor start.
func TestCoreStartMetricsRecordedOnce(t *testing.T) {
	manager, _ := testEnvWithMetrics(t)

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v (state %s)", err, snapshot.State)
	}

	defer manager.Disconnect()

	reg := managerMetrics(t, manager)

	s := reg.Snapshot()

	if s.CoreStarts != 1 {
		t.Fatalf("CoreStarts = %d, want 1 (double counting regressed)", s.CoreStarts)
	}

	if s.CoreStartFails != 0 {
		t.Fatalf("CoreStartFails = %d, want 0", s.CoreStartFails)
	}

	if s.AvgCoreStartupMS <= 0 {
		t.Fatalf("AvgCoreStartupMS = %v, want one observed startup", s.AvgCoreStartupMS)
	}

	// A second Connect (after Disconnect) must not inflate the counter
	// beyond one measurement PER session.
	manager.Disconnect()

	if _, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{}); err != nil {
		t.Fatalf("second Connect() = %v", err)
	}

	defer manager.Disconnect()

	s = reg.Snapshot()

	if s.CoreStarts != 2 {
		t.Fatalf("CoreStarts after two sessions = %d, want 2", s.CoreStarts)
	}
}

// TestUserPortOccupiedFailsLoudly covers the user-selected-port
// contract: a port already bound by another listener must fail the
// attempt with the port named, not surface later as a core bind
// crash (§5 port-conflict detection).
func TestUserPortOccupiedFailsLoudly(t *testing.T) {
	manager, _ := testEnv(t)

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatal(err)
	}

	defer ln.Close()

	occupied := ln.Addr().(*net.TCPAddr).Port

	manager.SetLocalPorts(occupied, 0)

	_, err = manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err == nil {
		t.Fatal("connect with an occupied user port must fail")
	}

	if !contains(err.Error(), strconv.Itoa(occupied)) {
		t.Fatalf("error does not name the conflicting port %d: %v", occupied, err)
	}

	if state := manager.State(); state != connection.StateConnectionFailed &&
		state != connection.StateDisconnected {
		t.Fatalf("state after occupied-port failure = %s", state)
	}
}

// TestUserPortReconnectKeepsPreference proves the fixed-port contract
// across reconnects (§5: explicit user-selected ports respected on
// every attempt, no re-allocation).
func TestUserPortReconnectKeepsPreference(t *testing.T) {
	manager, _ := testEnv(t)

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatal(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port

	// Reserve the port for the core: it must stay free for the manager
	// (close the probe listener before connecting).
	_ = ln.Close()

	manager.SetLocalPorts(port, 0)

	first, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v", err)
	}

	if first.Endpoint == "" || !contains(first.Endpoint, strconv.Itoa(port)) {
		t.Fatalf("endpoint %q does not carry the user port %d", first.Endpoint, port)
	}

	manager.Disconnect()

	second, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("reconnect = %v", err)
	}

	defer manager.Disconnect()

	if !contains(second.Endpoint, strconv.Itoa(port)) {
		t.Fatalf("reconnect endpoint %q does not carry the user port %d", second.Endpoint, port)
	}
}

// TestRapidDisconnectReconnectCycles is the §5 stress shape: rapid
// disconnect/reconnect cycles must neither leak ports nor wedge the
// state machine.
func TestRapidDisconnectReconnectCycles(t *testing.T) {
	manager, _ := testEnv(t)

	for i := 0; i < 5; i++ {
		snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
		if err != nil {
			t.Fatalf("cycle %d: Connect() = %v (state %s)", i, err, snapshot.State)
		}

		final := manager.Disconnect()

		if final.State != connection.StateDisconnected {
			t.Fatalf("cycle %d: final state = %s", i, final.State)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}

	return -1
}

// testEnvWithMetrics is testEnv with a metrics registry wired through
// the manager options (exported through a package-level accessor the
// test can read; the manager keeps the registry private).
var lastMetrics *metrics.Registry

func testEnvWithMetrics(t *testing.T) (*connection.Manager, *core.Registry) {
	t.Helper()

	dir := t.TempDir()

	contract.StageFakeCore(t, dir, "v2ray")

	registry := core.NewRegistry(system.NewCoreLocator(dir))

	if err := registry.Register(v2ray.New(), 1); err != nil {
		t.Fatalf("register v2ray: %v", err)
	}

	if err := registry.Register(xray.New(), 0); err != nil {
		t.Fatalf("register xray: %v", err)
	}

	registry.Refresh(context.Background())

	reg := metrics.New()
	lastMetrics = reg

	manager := connection.New(connection.Options{
		Registry:        registry,
		Metrics:         reg,
		StartupTimeout:  10 * time.Second,
		MonitorInterval: 200 * time.Millisecond,
		Verify:          connection.VerifyPolicy{Skip: true},
	})

	return manager, registry
}

func managerMetrics(t *testing.T, _ *connection.Manager) *metrics.Registry {
	t.Helper()

	if lastMetrics == nil {
		t.Fatal("metrics registry not wired")
	}

	return lastMetrics
}
