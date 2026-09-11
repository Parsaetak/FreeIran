package connection_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
	"github.com/Parsaetak/FreeIran/system"
)

// testEnv builds a connection manager wired to a registry with a
// fake v2ray executable staged for discovery.
func testEnv(t *testing.T) (*connection.Manager, *core.Registry) {
	t.Helper()

	fake := contract.BuildFakeCore(t)

	dir := t.TempDir()

	// Stage the fake binary as "v2ray".
	if err := copyFile(fake, dir+"/v2ray"); err != nil {
		t.Fatalf("stage fake v2ray: %v", err)
	}

	registry := core.NewRegistry(system.NewCoreLocator(dir))

	if err := registry.Register(v2ray.New(), 1); err != nil {
		t.Fatalf("register v2ray: %v", err)
	}

	if err := registry.Register(xray.New(), 0); err != nil {
		t.Fatalf("register xray: %v", err)
	}

	registry.Refresh(context.Background())

	manager := connection.New(connection.Options{
		Registry:        registry,
		StartupTimeout:  10 * time.Second,
		MonitorInterval: 200 * time.Millisecond,
	})

	return manager, registry
}

// testRegistry builds the discovery-backed registry shared by the
// connection tests (fake v2ray staged, xray registered but missing).
func testRegistry(t *testing.T) *core.Registry {
	t.Helper()

	fake := contract.BuildFakeCore(t)

	dir := t.TempDir()

	if err := copyFile(fake, dir+"/v2ray"); err != nil {
		t.Fatalf("stage fake v2ray: %v", err)
	}

	registry := core.NewRegistry(system.NewCoreLocator(dir))

	if err := registry.Register(v2ray.New(), 1); err != nil {
		t.Fatalf("register v2ray: %v", err)
	}

	if err := registry.Register(xray.New(), 0); err != nil {
		t.Fatalf("register xray: %v", err)
	}

	registry.Refresh(context.Background())

	return registry
}

func vlessTestConfig() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Name:     "test-vless",
		Address:  "conn.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "tcp",
		Security: "tls",
	}
}

// TestConnectDisconnect verifies the happy path state machine:
// disconnected → selecting → preparing → starting → ready →
// connected → disconnecting → disconnected.
func TestConnectDisconnect(t *testing.T) {
	manager, _ := testEnv(t)

	if state := manager.State(); state != connection.StateDisconnected {
		t.Fatalf("initial state = %s", state)
	}

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v (state %s)", err, snapshot.State)
	}

	if snapshot.State != connection.StateConnected {
		t.Fatalf("state = %s, want connected", snapshot.State)
	}

	if snapshot.Core != "v2ray" {
		t.Fatalf("core = %s, want v2ray (the available backend)", snapshot.Core)
	}

	if snapshot.ConfigName != "test-vless" {
		t.Fatalf("config name = %s", snapshot.ConfigName)
	}

	if snapshot.Endpoint == "" {
		t.Fatal("endpoint is empty")
	}

	if snapshot.LatencyMS < 0 {
		t.Fatalf("negative latency: %d", snapshot.LatencyMS)
	}

	// The display string must be credential-free.
	if strings.Contains(snapshot.ConfigDisplay, "11111111-1111") {
		t.Fatalf("snapshot leaks credential: %s", snapshot.ConfigDisplay)
	}

	// Health report axes.
	health := manager.Health(context.Background())
	if !health.ProcessAlive || !health.ListenerReady {
		t.Fatalf("health = %+v", health)
	}

	final := manager.Disconnect()

	if final.State != connection.StateDisconnected {
		t.Fatalf("final state = %s", final.State)
	}

	// Second disconnect is a no-op.
	if again := manager.Disconnect(); again.State != connection.StateDisconnected {
		t.Fatalf("second disconnect state = %s", again.State)
	}
}

// TestConnectRejectsConcurrent verifies one active session.
func TestConnectRejectsConcurrent(t *testing.T) {
	manager, _ := testEnv(t)

	_, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("first Connect() = %v", err)
	}

	defer manager.Disconnect()

	_, err = manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err == nil {
		t.Fatal("second concurrent Connect() should fail")
	}

	if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestConnectNoBackend verifies the clean failure path.
func TestConnectNoBackend(t *testing.T) {
	// Registry with a nil locator: nothing is available.
	registry := core.NewRegistry(nil)

	if err := registry.Register(v2ray.New(), 1); err != nil {
		t.Fatalf("register: %v", err)
	}

	manager := connection.New(connection.Options{Registry: registry})

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err == nil {
		t.Fatal("Connect() without an available backend should fail")
	}

	if snapshot.State != connection.StateConnectionFailed {
		t.Fatalf("state = %s, want connection_failed", snapshot.State)
	}

	if snapshot.LastError == "" {
		t.Fatal("last error is empty")
	}
}

// TestReconnect verifies reconnect re-establishes the session.
func TestReconnect(t *testing.T) {
	manager, _ := testEnv(t)

	_, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v", err)
	}

	manager.Disconnect()

	snapshot, err := manager.Reconnect(context.Background())
	if err != nil {
		t.Fatalf("Reconnect() = %v", err)
	}

	if snapshot.State != connection.StateConnected {
		t.Fatalf("state = %s, want connected", snapshot.State)
	}

	manager.Disconnect()
}

// TestReconnectWithoutPrevious verifies the guard.
func TestReconnectWithoutPrevious(t *testing.T) {
	manager, _ := testEnv(t)

	if _, err := manager.Reconnect(context.Background()); err == nil {
		t.Fatal("Reconnect() without a previous session should fail")
	}
}

// TestShutdownIsTerminal verifies shutdown disables the manager.
func TestShutdownIsTerminal(t *testing.T) {
	manager, _ := testEnv(t)

	_, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v", err)
	}

	manager.Shutdown()

	if state := manager.State(); state != connection.StateDisconnected {
		t.Fatalf("state after shutdown = %s", state)
	}

	_, err = manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err == nil {
		t.Fatal("Connect() after shutdown should fail")
	}

	if !strings.Contains(err.Error(), "shut down") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAttemptHistory verifies attempts are recorded with reasons
// (§41 observability): selection failures carry an explainable
// LastError, and backend launch failures produce attempt records.
func TestAttemptHistory(t *testing.T) {
	manager, _ := testEnv(t)

	// Selection failure: a REALITY config with only v2ray available
	// (REALITY-incompatible) resolves to no candidate — the failure
	// reason is the selection explanation, with no attempts yet.
	reality := vlessTestConfig()
	reality.Security = "reality"
	reality.PublicKey = "49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY"

	snapshot, err := manager.Connect(context.Background(), reality, core.Preferences{
		AllowFallback: true,
	})
	if err == nil {
		t.Fatal("REALITY connect with only v2ray available should fail")
	}

	if snapshot.State != connection.StateConnectionFailed {
		t.Fatalf("state = %s", snapshot.State)
	}

	if snapshot.LastError == "" {
		t.Fatal("selection failure reason is empty")
	}

	// The error must never leak the UUID.
	if strings.Contains(snapshot.LastError, "11111111-1111") {
		t.Fatalf("last error leaks credential: %s", snapshot.LastError)
	}

	// Launch failure: a failing fake core produces an attempt record
	// carrying the backend name and failure reason.
	failing := connection.New(connection.Options{
		Registry:        testRegistry(t),
		StartupTimeout:  10 * time.Second,
		MonitorInterval: 200 * time.Millisecond,
		LaunchEnv:       []string{"FAKECORE_FAIL_FAST=1"},
	})

	snapshot, err = failing.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err == nil {
		t.Fatal("connect against a failing core should fail")
	}

	if len(snapshot.Attempts) == 0 {
		t.Fatalf("no attempt records (last error: %s)", snapshot.LastError)
	}

	attempt := snapshot.Attempts[0]

	if attempt.Backend != "v2ray" || attempt.OK {
		t.Fatalf("attempt = %+v, want failed v2ray", attempt)
	}

	if attempt.Error == "" {
		t.Fatal("attempt failure reason is empty")
	}

	if strings.Contains(attempt.Error, "11111111-1111") {
		t.Fatalf("attempt error leaks credential: %s", attempt.Error)
	}
}

// TestSnapshotCredentials verifies the snapshot stays credential-free.
func TestSnapshotCredentials(t *testing.T) {
	manager, _ := testEnv(t)

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v", err)
	}

	defer manager.Disconnect()

	if strings.Contains(snapshot.ConfigDisplay, "11111111-1111-1111-1111-111111111111") {
		t.Fatalf("snapshot display leaks credential: %s", snapshot.ConfigDisplay)
	}
}

// TestRapidConnectDisconnect hammers the lifecycle (§44).
func TestRapidConnectDisconnect(t *testing.T) {
	manager, _ := testEnv(t)

	for cycle := 0; cycle < 3; cycle++ {
		if _, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{}); err != nil {
			t.Fatalf("cycle %d: Connect() = %v", cycle, err)
		}

		manager.Disconnect()
	}
}

// TestConnectContextCancellation verifies cancellation aborts.
func TestConnectContextCancellation(t *testing.T) {
	manager, _ := testEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := manager.Connect(ctx, vlessTestConfig(), core.Preferences{})
	if err == nil {
		t.Fatal("cancelled context should fail the connection")
	}
}

// TestStateHelpers verifies state classification helpers.
func TestStateHelpers(t *testing.T) {
	if !connection.StateConnected.Active() {
		t.Fatal("connected should be active")
	}

	if connection.StateDisconnected.Active() {
		t.Fatal("disconnected should not be active")
	}

	if !connection.StateDisconnected.Terminal() {
		t.Fatal("disconnected should be terminal")
	}

	if connection.StateSelecting.Terminal() {
		t.Fatal("selecting should not be terminal")
	}
}

// TestNilManagerSafety verifies nil guards.
func TestNilManagerSafety(t *testing.T) {
	var manager *connection.Manager

	if manager.State() != connection.StateDisconnected {
		t.Fatal("nil manager state")
	}

	if manager.Snapshot().State != connection.StateDisconnected {
		t.Fatal("nil manager snapshot")
	}

	manager.Disconnect()
	manager.Shutdown()

	_, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err == nil {
		t.Fatal("nil manager Connect should fail")
	}

	if !errors.Is(err, err) {
		t.Fatal("error identity")
	}
}

// copyFile duplicates a file with executable permissions.
func copyFile(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}

	return os.WriteFile(target, data, 0o755)
}
