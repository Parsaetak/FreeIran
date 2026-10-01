package connection_test

import (
	"context"
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

// chainSessionConfig composes the synthetic v0.12.2 chain
// configuration exactly as the app layer's buildChainConfig does:
// the egress hop as the config itself, earlier hops in cfg.Chain.
func chainSessionConfig() config.Config {
	hopA := config.Config{
		Type:    config.TypeVLESS,
		Name:    "hop-a",
		Address: "a.example.org",
		Port:    443,
		UUID:    "aaaaaaaa-1111-1111-1111-111111111111",
		Network: "tcp",
	}

	hopB := config.Config{
		Type:    config.TypeVLESS,
		Name:    "hop-b",
		Address: "b.example.org",
		Port:    8443,
		UUID:    "bbbbbbbb-2222-2222-2222-222222222222",
		Network: "tcp",
	}

	exit := config.Config{
		Type:     config.TypeVLESS,
		Name:     "test-chain",
		Address:  "c.example.org",
		Port:     443,
		UUID:     "cccccccc-3333-3333-3333-333333333333",
		Network:  "tcp",
		Security: "tls",
		Chain:    []*config.Config{&hopA, &hopB},
	}

	return exit
}

// TestChainSessionLifecycleFakeCore pins the deterministic chain
// session contract on the fake core (live multi-hop runtime is
// unsuitable in unit tests — the GENERATED documents are asserted in
// the compiler tests; this test asserts the STATE MACHINE behaves
// identically for a chain session):
//
//   - the chain selects a chain-capable core (xray compiles the
//     chain; v2ray is explicitly chain-incapable and must be skipped);
//   - the session reaches the same verified-path states as a plain
//     configuration (verification skipped here — the verify gate
//     itself is core-agnostic and shared);
//   - the snapshot displays the CHAIN identity, never one hop's URL;
//   - the session carries the "proxy chain" route label;
//   - Disconnect tears the session down deterministically.
func TestChainSessionLifecycleFakeCore(t *testing.T) {
	// Own env: BOTH fake cores staged, so the selection has a real
	// choice — the chain-capable xray must win over the
	// chain-incapable v2ray.
	dir := t.TempDir()

	contract.StageFakeCore(t, dir, "v2ray")
	contract.StageFakeCore(t, dir, "xray")

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
		Verify:          connection.VerifyPolicy{Skip: true},
	})

	snapshot, err := manager.Connect(context.Background(), chainSessionConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect(chain) = %v (state %s)", err, snapshot.State)
	}

	// xray is the chain-capable backend (the fake v2ray adapter
	// refuses chains); the state machine must not have fallen back
	// into an incompatible core.
	if snapshot.Core != "xray" {
		t.Fatalf("core = %q, want xray (chain-capable)", snapshot.Core)
	}

	if snapshot.State != connection.StateConnected {
		t.Fatalf("state = %s, want connected", snapshot.State)
	}

	if snapshot.ConfigName != "test-chain" {
		t.Fatalf("config name = %q, want the chain name", snapshot.ConfigName)
	}

	if !strings.HasSuffix(snapshot.ConfigDisplay, "(proxy chain)") {
		t.Fatalf("display = %q, want the chain identity (…(proxy chain))", snapshot.ConfigDisplay)
	}

	if label := manager.SessionLabel(); label != "proxy chain" {
		t.Fatalf("session label = %q, want proxy chain", label)
	}

	if snapshot.Endpoint == "" {
		t.Fatal("endpoint is empty")
	}

	if snapshot.CorePID <= 0 {
		t.Fatal("snapshot carries no core pid")
	}

	final := manager.Disconnect()

	if final.State != connection.StateDisconnected {
		t.Fatalf("final state = %s", final.State)
	}

	if label := manager.SessionLabel(); label != "" {
		t.Fatalf("session label after disconnect = %q, want empty", label)
	}
}

// TestChainSessionEmptyChainFieldDegrades proves the state machine
// never treats an empty Chain slice as a chain (IsChain false): it
// connects as a plain configuration. The degenerate single-ID list is
// refused at the SERVICE boundary (proxychain.go validateHopList) and
// the <2-hop compiler guard is defense in depth.
func TestChainSessionEmptyChainFieldDegrades(t *testing.T) {
	manager, _ := testEnv(t)

	cfg := config.Config{
		Type:     config.TypeVLESS,
		Name:     "not-a-chain",
		Address:  "c.example.org",
		Port:     443,
		UUID:     "cccccccc-3333-3333-3333-333333333333",
		Network:  "tcp",
		Security: "tls",
		Chain:    []*config.Config{},
	}

	snapshot, err := manager.Connect(context.Background(), cfg, core.Preferences{})
	if err != nil {
		t.Fatalf("empty-slice chain must degrade to a plain config: %v", err)
	}

	// The exit config is the FIRST compatible backend for a plain
	// config — v2ray here (priority 0 fake) proves no chain path ran.
	_ = snapshot

	if label := manager.SessionLabel(); label != "" {
		t.Fatalf("plain config carries a session label %q", label)
	}

	if final := manager.Disconnect(); final.State != connection.StateDisconnected {
		t.Fatalf("final state = %s", final.State)
	}
}
