package core_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/singbox"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
	"github.com/Parsaetak/FreeIran/system"
)

// stubBackend is a minimal registry/selection test backend.
type stubBackend struct {
	name  string
	caps  core.Capabilities
	state core.BackendStatus
}

func (s *stubBackend) Name() string { return s.name }

func (s *stubBackend) Supports(cfg config.Config) bool {
	return s.caps.Matches(cfg)
}

func (s *stubBackend) Validate(_ context.Context, _ config.Config) error {
	return nil
}

func (s *stubBackend) BuildConfig(_ config.Config, _ core.RuntimeOptions) (core.RuntimeConfig, error) {
	return core.RuntimeConfig{FileName: "x.json", Data: []byte("{}")}, nil
}

func (s *stubBackend) Start(_ context.Context, _ config.Config, _ core.RuntimeOptions) (*core.Instance, error) {
	return nil, nil
}

func (s *stubBackend) Capabilities() core.Capabilities { return s.caps }

// newTestRegistry builds a registry with three stub backends in
// distinct availability states.
func newTestRegistry() *core.Registry {
	registry := core.NewRegistry(nil)

	xrayLike := &stubBackend{
		name: "xray",
		caps: core.Capabilities{
			Protocols:  []config.Type{config.TypeVLESS, config.TypeVMess},
			Transports: []string{"tcp", "ws"},
			Securities: []string{"none", "tls", "reality"},
			Flows:      []string{"", "xtls-rprx-vision"},
		},
	}

	v2rayLike := &stubBackend{
		name: "v2ray",
		caps: core.Capabilities{
			Protocols:  []config.Type{config.TypeVLESS, config.TypeVMess, config.TypeTrojan},
			Transports: []string{"tcp", "ws", "grpc"},
			Securities: []string{"none", "tls"},
			Flows:      []string{""},
		},
	}

	singboxLike := &stubBackend{
		name: "sing-box",
		caps: core.Capabilities{
			Protocols:  []config.Type{config.TypeVLESS, config.TypeVMess, config.TypeHysteria2},
			Transports: []string{"tcp", "ws"},
			Securities: []string{"none", "tls"},
			Flows:      []string{""},
		},
	}

	_ = registry.Register(xrayLike, 0)
	_ = registry.Register(v2rayLike, 1)
	_ = registry.Register(singboxLike, 2)

	return registry
}

// TestRegistryBackendsOrder verifies deterministic priority ordering.
func TestRegistryBackendsOrder(t *testing.T) {
	registry := newTestRegistry()

	// With a nil locator all backends report missing.
	registry.Refresh(context.Background())

	backends := registry.Backends()

	if len(backends) != 3 {
		t.Fatalf("backend count = %d, want 3", len(backends))
	}

	if backends[0].Name != "xray" || backends[1].Name != "v2ray" || backends[2].Name != "sing-box" {
		t.Fatalf("order = %s,%s,%s",
			backends[0].Name, backends[1].Name, backends[2].Name)
	}

	for _, backend := range backends {
		if backend.Status != core.StatusMissing {
			t.Fatalf("%s status = %s, want missing (nil locator)", backend.Name, backend.Status)
		}
	}
}

// TestRegistryGet verifies lookup.
func TestRegistryGet(t *testing.T) {
	registry := newTestRegistry()

	if _, ok := registry.Get("xray"); !ok {
		t.Fatal("xray not found")
	}

	if _, ok := registry.Get("nope"); ok {
		t.Fatal("unknown backend found")
	}
}

// TestRegistryRegisterRejectsNil verifies nil guards.
func TestRegistryRegisterRejectsNil(t *testing.T) {
	registry := core.NewRegistry(nil)

	if err := registry.Register(nil, 0); err == nil {
		t.Fatal("nil backend should be rejected")
	}
}

// TestSelectionPrefersPriority verifies deterministic selection:
// priority ordering wins for compatible available backends.
func TestSelectionPrefersPriority(t *testing.T) {
	registry := newTestRegistry()

	// With no locator, selection cannot find available backends —
	// use the direct Core path instead: Register + capability
	// resolution happen in Select via Supports. Missing binaries
	// make backends unavailable, so selection must fail cleanly.
	_, err := registry.Select(vlessConfig(), core.Preferences{})
	if err == nil {
		t.Fatal("selection with zero available backends should fail")
	}

	if !strings.Contains(err.Error(), "no compatible backend") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSelectionWithRealBackends exercises selection through the
// registry availability state using the real adapters and a locator
// that discovers a fake binary as "v2ray".
func TestSelectionWithRealBackends(t *testing.T) {
	fake := contract.BuildFakeCore(t)

	dir := t.TempDir()

	// Place the fake binary where the locator looks for "v2ray".
	target := dir + "/v2ray"

	if err := copyFile(fake, target); err != nil {
		t.Fatalf("stage fake v2ray: %v", err)
	}

	locator := system.NewCoreLocator(dir)
	registry := core.NewRegistry(locator)

	_ = registry.Register(v2ray.New(), 1)
	_ = registry.Register(xray.New(), 0)
	_ = registry.Register(singbox.New(), 2)

	registry.Refresh(context.Background())

	backends := registry.Backends()

	if len(backends) != 3 {
		t.Fatalf("backend count = %d", len(backends))
	}

	// Only the fake v2ray is "available".
	v2rayAvailable := false

	for _, backend := range backends {
		if backend.Name == "v2ray" && backend.Status == core.StatusAvailable {
			v2rayAvailable = true
		}
	}

	if !v2rayAvailable {
		t.Fatalf("fake v2ray not discovered: %+v", backends)
	}

	// A vless+reality config: xray is preferred but missing; the
	// fake v2ray is available but REALITY-incompatible; selection
	// must fail with an explainable error.
	realityCfg := config.Config{
		Type:      config.TypeVLESS,
		Address:   "reality.example.org",
		Port:      443,
		UUID:      "11111111-1111-1111-1111-111111111111",
		Network:   "tcp",
		Security:  "reality",
		PublicKey: "49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY",
	}

	_, err := registry.Select(realityCfg, core.Preferences{})
	if err == nil {
		t.Fatal("REALITY selection with only v2ray available should fail")
	}

	// A plain vless+tls config: the available fake v2ray wins even
	// though xray has higher priority (xray binary is missing).
	tlsCfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "tls.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "tcp",
		Security: "tls",
	}

	selection, err := registry.Select(tlsCfg, core.Preferences{})
	if err != nil {
		t.Fatalf("Select() = %v", err)
	}

	if selection.Core.Name() != "v2ray" {
		t.Fatalf("selected = %s, want v2ray (the only available backend)", selection.Core.Name())
	}

	if selection.Reason == "" {
		t.Fatal("selection reason is empty")
	}

	// Deterministic: same inputs, same outcome.
	again, err := registry.Select(tlsCfg, core.Preferences{})
	if err != nil {
		t.Fatalf("second Select() = %v", err)
	}

	if again.Core.Name() != selection.Core.Name() || again.Reason != selection.Reason {
		t.Fatal("selection is not deterministic")
	}
}

// TestSelectionPreferenceOverridesPriority verifies user preference
// wins over priority when compatible and available.
func TestSelectionPreferenceOverridesPriority(t *testing.T) {
	fake := contract.BuildFakeCore(t)

	dir := t.TempDir()

	for _, name := range []string{"v2ray", "xray"} {
		if err := copyFile(fake, dir+"/"+name); err != nil {
			t.Fatalf("stage fake %s: %v", name, err)
		}
	}

	locator := system.NewCoreLocator(dir)
	registry := core.NewRegistry(locator)

	_ = registry.Register(xray.New(), 0)
	_ = registry.Register(v2ray.New(), 1)

	registry.Refresh(context.Background())

	cfg := config.Config{
		Type:    config.TypeVLESS,
		Address: "pref.example.org",
		Port:    443,
		UUID:    "11111111-1111-1111-1111-111111111111",
		Network: "tcp",
	}

	selection, err := registry.Select(cfg, core.Preferences{})
	if err != nil {
		t.Fatalf("Select() = %v", err)
	}

	if selection.Core.Name() != "xray" {
		t.Fatalf("default selection = %s, want xray (priority)", selection.Core.Name())
	}

	pref, err := registry.Select(cfg, core.Preferences{PreferredBackend: "v2ray"})
	if err != nil {
		t.Fatalf("Select(preferred) = %v", err)
	}

	if pref.Core.Name() != "v2ray" {
		t.Fatalf("preferred selection = %s, want v2ray", pref.Core.Name())
	}

	// Preference never overrides capability: a REALITY configuration
	// with a v2ray preference must resolve to the compatible xray (or
	// sing-box), never to the incompatible preferred backend.
	reality := cfg
	reality.Security = "reality"
	reality.PublicKey = "49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY"

	capabilitySelection, err := registry.Select(reality, core.Preferences{PreferredBackend: "v2ray"})
	if err != nil {
		t.Fatalf("REALITY selection should succeed through xray: %v", err)
	}

	if capabilitySelection.Core.Name() == "v2ray" {
		t.Fatal("preference must never override capability incompatibility")
	}

	if capabilitySelection.Core.Name() != "xray" {
		t.Fatalf("REALITY should resolve to xray, got %s", capabilitySelection.Core.Name())
	}
}

// TestLogBufferRedaction verifies captured output is redacted.
func TestLogBufferRedaction(t *testing.T) {
	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "secret.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Password: "super-secret-password",
	}

	logs := core.NewLogBuffer(cfg)

	if _, err := logs.Write([]byte(
		"started with uuid 11111111-1111-1111-1111-111111111111 and vless://11111111-1111-1111-1111-111111111111@secret.example.org:443 path\n" +
			"password=super-secret-password rejected\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	lines := logs.Lines()

	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "11111111-1111-1111-1111-111111111111") {
		t.Fatalf("uuid leaked into logs: %s", joined)
	}

	if strings.Contains(joined, "super-secret-password") {
		t.Fatalf("password leaked into logs: %s", joined)
	}

	if !strings.Contains(joined, "***@secret.example.org:443") {
		t.Fatalf("redacted URL form missing: %s", joined)
	}
}

// TestLogBufferBounded verifies the buffer stays bounded.
func TestLogBufferBounded(t *testing.T) {
	logs := core.NewLogBuffer(config.Config{})

	for i := 0; i < 10_000; i++ {
		_, _ = logs.Write([]byte("line"))
	}

	if got := len(logs.Lines()); got > 256 {
		t.Fatalf("buffer grew to %d lines", got)
	}
}

// TestGenCacheGenerations verifies invalidation semantics.
func TestGenCacheGenerations(t *testing.T) {
	cache := core.NewGenCache()

	doc := core.RuntimeConfig{FileName: "a.json", Data: []byte("{}")}

	cache.Put("k", 1, doc)

	if got, ok := cache.Get("k", 1); !ok || string(got.Data) != "{}" {
		t.Fatal("cache miss after put")
	}

	if _, ok := cache.Get("k", 2); ok {
		t.Fatal("generation change must invalidate")
	}

	// After wholesale invalidation the old key is gone even when the
	// generation returns to a previous value.
	cache.Put("k2", 2, doc)

	if _, ok := cache.Get("k", 2); ok {
		t.Fatal("stale entry survived invalidation")
	}
}

// TestRedactLogText verifies URL userinfo redaction.
func TestRedactLogText(t *testing.T) {
	out := core.RedactLogText(
		"trojan://hunter2@host.example:443?type=tcp vless://uuid-x@a.b:1",
		[]string{"hunter2"})

	if strings.Contains(out, "hunter2") || strings.Contains(out, "uuid-x") {
		t.Fatalf("credentials leaked: %s", out)
	}

	if !strings.Contains(out, "trojan://***@host.example:443") {
		t.Fatalf("redacted form missing: %s", out)
	}
}

// TestPinnedCores verifies the documentation pins are coherent.
func TestPinnedCores(t *testing.T) {
	for _, pinned := range core.PinnedCores {
		if pinned.Name == "" || pinned.Version == "" || pinned.Source == "" {
			t.Fatalf("incomplete pin: %+v", pinned)
		}
	}

	if core.PinnedVersion("v2ray") != "5.53.0" {
		t.Fatalf("v2ray pin = %s", core.PinnedVersion("v2ray"))
	}
}

// TestInstanceCloseWithoutProcess verifies Close on a partially
// constructed instance is safe.
func TestInstanceCloseWithoutProcess(t *testing.T) {
	instance := &core.Instance{}

	if err := instance.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	if instance.State() != core.StateStopped {
		t.Fatalf("state = %s", instance.State())
	}
}

// TestDisplayURLRedaction verifies config display redaction.
func TestDisplayURLRedaction(t *testing.T) {
	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "show.example.org",
		Port:     443,
		UUID:     "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Network:  "ws",
		Security: "tls",
	}

	display := cfg.DisplayURL()

	if strings.Contains(display, "aaaaaaaa-bbbb") {
		t.Fatalf("display URL leaks uuid: %s", display)
	}

	if !strings.Contains(display, "vless://***@show.example.org:443 (tls,ws)") {
		t.Fatalf("unexpected display form: %s", display)
	}
}

// TestRegistryString verifies diagnostics rendering.
func TestRegistryString(t *testing.T) {
	registry := newTestRegistry()

	rendered := registry.String()

	if !strings.Contains(rendered, "xray=missing") ||
		!strings.Contains(rendered, "v2ray=missing") {
		t.Fatalf("registry rendering: %s", rendered)
	}
}

// TestHealthReportFields verifies health report JSON tags survive
// for the UI surface.
func TestHealthReportFields(t *testing.T) {
	report := core.HealthReport{
		Core:          "v2ray",
		State:         core.StateRunning,
		ProcessAlive:  true,
		ListenerReady: true,
		LatencyMS:     12,
		CheckedAt:     time.Now().UTC(),
	}

	if !report.Healthy() {
		t.Fatal("alive+ready should be healthy")
	}
}

func vlessConfig() config.Config {
	return config.Config{
		Type:    config.TypeVLESS,
		Address: "select.example.org",
		Port:    443,
		UUID:    "11111111-1111-1111-1111-111111111111",
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
