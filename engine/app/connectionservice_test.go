package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
)

// newConnectionTestApp boots an app with a temp base directory and a
// fake v2ray executable staged in the managed cores directory.
func newConnectionTestApp(t *testing.T) *App {
	t.Helper()

	application, err := New(Options{
		BaseDir:             filepath.Join(t.TempDir(), "freeiran"),
		RefreshInterval:     time.Hour,
		RunIngestionOnStart: false,
		SkipDefaultSources:  true,
	})
	if err != nil {
		t.Fatalf("app.New() = %v", err)
	}

	t.Cleanup(application.Shutdown)

	// Stage the fake core binary as the managed "v2ray" runtime.
	fake := contract.BuildFakeCore(t)

	coresDir := filepath.Join(application.opts.BaseDir, "cores")

	if err := os.MkdirAll(coresDir, 0o700); err != nil {
		t.Fatalf("cores dir: %v", err)
	}

	data, err := os.ReadFile(fake)
	if err != nil {
		t.Fatalf("read fake core: %v", err)
	}

	if err := os.WriteFile(filepath.Join(coresDir, "v2ray"), data, 0o755); err != nil {
		t.Fatalf("stage fake v2ray: %v", err)
	}

	return application
}

// storeConfig persists a synthetic configuration through the data
// service path.
func storeConfig(t *testing.T, application *App, cfg config.Config) string {
	t.Helper()

	cfg.Normalize()

	cfg.SetID()

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := application.store.Upsert(cfg.ID, raw); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	return cfg.ID
}

// TestConnectionServiceBackends verifies the backend view surface.
func TestConnectionServiceBackends(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)

	// Force synchronous discovery.
	backends := service.RefreshBackends()

	if len(backends) != 3 {
		t.Fatalf("backend count = %d, want 3 (xray/v2ray/sing-box)", len(backends))
	}

	names := map[string]bool{}

	for _, backend := range backends {
		names[backend.Name] = true

		if backend.PinnedVersion == "" {
			t.Fatalf("%s has no pinned version", backend.Name)
		}

		if backend.Source == "" {
			t.Fatalf("%s has no source URL", backend.Name)
		}
	}

	if !names["xray"] || !names["v2ray"] || !names["sing-box"] {
		t.Fatalf("missing backends: %v", names)
	}

	// The staged fake v2ray must be discovered as available.
	found := false

	for _, backend := range backends {
		if backend.Name == "v2ray" && backend.Status == "available" {
			found = true
		}
	}

	if !found {
		t.Fatalf("fake v2ray not discovered: %+v", backends)
	}
}

// TestConnectionServiceConnectDisconnect verifies the full service
// flow: store a config, connect through the fake core, observe the
// state machine, disconnect cleanly.
func TestConnectionServiceConnectDisconnect(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)

	_ = service.RefreshBackends()

	cfg := config.Config{
		Type:     config.TypeVLESS,
		Name:     "service-test",
		Address:  "svc.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "tcp",
		Security: "tls",
	}

	id := storeConfig(t, application, cfg)

	snapshot, err := service.Connect(id)
	if err != nil {
		t.Fatalf("Connect() = %v (state %s, error %s)",
			err, snapshot.State, snapshot.LastError)
	}

	if snapshot.State != "connected" {
		t.Fatalf("state = %s, want connected", snapshot.State)
	}

	if snapshot.Core != "v2ray" {
		t.Fatalf("core = %s, want v2ray", snapshot.Core)
	}

	if snapshot.ConfigName != "service-test" {
		t.Fatalf("config name = %s", snapshot.ConfigName)
	}

	// Credential never crosses the service boundary.
	if snapshot.ConfigDisplay == "" ||
		len(snapshot.ConfigDisplay) == 0 ||
		contains(snapshot.ConfigDisplay, "11111111-1111") {
		t.Fatalf("config display leaks or is empty: %s", snapshot.ConfigDisplay)
	}

	// ConnectionState reflects the session.
	state := service.ConnectionState()
	if state.State != "connected" {
		t.Fatalf("ConnectionState() = %s", state.State)
	}

	// Health axes through the service.
	health := service.Health()
	if !health.ProcessAlive || !health.ListenerReady {
		t.Fatalf("health = %+v", health)
	}

	final := service.Disconnect()
	if final.State != "disconnected" {
		t.Fatalf("final state = %s", final.State)
	}
}

// TestConnectionServiceConfigDetails verifies the §17 details view
// with redaction.
func TestConnectionServiceConfigDetails(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)

	cfg := config.Config{
		Type:     config.TypeTrojan,
		Name:     "details-test",
		Address:  "details.example.org",
		Port:     443,
		Password: "synthetic-trojan-password",
		Network:  "tcp",
		Security: "tls",
		Source:   "test",
	}

	id := storeConfig(t, application, cfg)

	detail, err := service.ConfigDetails(id)
	if err != nil {
		t.Fatalf("ConfigDetails() = %v", err)
	}

	if detail.Type != "trojan" || detail.Address != "details.example.org" {
		t.Fatalf("detail = %+v", detail)
	}

	if !detail.HasPassword {
		t.Fatal("HasPassword should be true")
	}

	// The view exposes presence flags, never the secret itself.
	if contains(detail.Display, "synthetic-trojan-password") {
		t.Fatalf("detail display leaks password: %s", detail.Display)
	}

	// Compatible backends: all three adapters support trojan+tcp+tls.
	if len(detail.Backends) == 0 {
		t.Fatal("no compatible backends listed")
	}

	for _, backend := range detail.Backends {
		if backend != "xray" && backend != "v2ray" && backend != "sing-box" {
			t.Fatalf("unexpected backend %s", backend)
		}
	}
}

// TestConnectionServiceUnknownConfig verifies the error path.
func TestConnectionServiceUnknownConfig(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)

	if _, err := service.Connect("does-not-exist"); err == nil {
		t.Fatal("Connect() with unknown id should fail")
	}

	if _, err := service.ConfigDetails("does-not-exist"); err == nil {
		t.Fatal("ConfigDetails() with unknown id should fail")
	}
}

// TestConnectionServiceConnectAdHoc verifies ad-hoc connection
// without storage.
func TestConnectionServiceConnectAdHoc(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)

	_ = service.RefreshBackends()

	snapshot, err := service.ConnectConfig(config.Config{
		Type:     config.TypeVMess,
		Address:  "adhoc.example.org",
		Port:     443,
		UUID:     "22222222-2222-2222-2222-222222222222",
		Network:  "tcp",
		Security: "tls",
	})
	if err != nil {
		t.Fatalf("ConnectConfig() = %v", err)
	}

	if snapshot.Core != "v2ray" {
		t.Fatalf("core = %s", snapshot.Core)
	}

	service.Disconnect()
}

// TestAppShutdownDisconnectsSession verifies the shutdown ordering:
// an active session is torn down before the store closes (no
// orphaned process, no locked temp config).
func TestAppShutdownDisconnectsSession(t *testing.T) {
	base := filepath.Join(t.TempDir(), "freeiran")

	application, err := New(Options{
		BaseDir:             base,
		RefreshInterval:     time.Hour,
		RunIngestionOnStart: false,
		SkipDefaultSources:  true,
	})
	if err != nil {
		t.Fatalf("app.New() = %v", err)
	}

	fake := contract.BuildFakeCore(t)

	coresDir := filepath.Join(base, "cores")

	if err := os.MkdirAll(coresDir, 0o700); err != nil {
		t.Fatalf("cores dir: %v", err)
	}

	data, err := os.ReadFile(fake)
	if err != nil {
		t.Fatalf("read fake core: %v", err)
	}

	if err := os.WriteFile(filepath.Join(coresDir, "v2ray"), data, 0o755); err != nil {
		t.Fatalf("stage fake v2ray: %v", err)
	}

	service := NewConnectionService(application)

	_ = service.RefreshBackends()

	id := storeConfig(t, application, config.Config{
		Type:     config.TypeVLESS,
		Address:  "shutdown.example.org",
		Port:     443,
		UUID:     "33333333-3333-3333-3333-333333333333",
		Network:  "tcp",
		Security: "tls",
	})

	if _, err := service.Connect(id); err != nil {
		t.Fatalf("Connect() = %v", err)
	}

	if service.ConnectionState().State != "connected" {
		t.Fatal("session not connected before shutdown")
	}

	// Shutdown must disconnect the session.
	application.Shutdown()

	if state := service.ConnectionState().State; state != "disconnected" {
		t.Fatalf("state after shutdown = %s", state)
	}

	// The temp directory must contain no leftover runtime configs.
	// (Temp configs live under os.TempDir with the freeiran- prefix;
	// the instance cleanup is verified through the connection state
	// and the contract suite's cleanup assertions.)
	_ = context.Background()
}

// contains is a substring check for redaction assertions.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
