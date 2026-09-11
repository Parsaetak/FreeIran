package core_test

import (
	"context"
	"os"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/singbox"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
	"github.com/Parsaetak/FreeIran/system"
)

// benchRegistry builds a registry whose three backends all resolve
// to the same fake executable (selection cost is what is measured).
func benchRegistry(b *testing.B) *core.Registry {
	b.Helper()

	dir := b.TempDir()

	// A stand-in executable that responds to nothing quickly.
	standin := dir + "/xray"

	if err := writeExecutable(standin); err != nil {
		b.Fatal(err)
	}

	locator := system.NewCoreLocator(dir)
	registry := core.NewRegistry(locator)

	if err := registry.Register(xray.New(), 0); err != nil {
		b.Fatal(err)
	}

	if err := registry.Register(v2ray.New(), 1); err != nil {
		b.Fatal(err)
	}

	if err := registry.Register(singbox.New(), 2); err != nil {
		b.Fatal(err)
	}

	// Discovery over the stand-in: whatever availability results, the
	// benchmark measures the full deterministic resolution path.
	registry.Refresh(context.Background())

	return registry
}

// BenchmarkBackendSelection measures deterministic backend selection
// across three registered backends.
func BenchmarkBackendSelection(b *testing.B) {
	registry := benchRegistry(b)

	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "bench.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "ws",
		Security: "tls",
	}

	pref := core.Preferences{AllowFallback: true}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Outcome (selection or explainable failure) is irrelevant
		// here; the resolution cost is what is measured.
		_, _ = registry.Select(cfg, pref)
	}
}

// BenchmarkBackendSelectionAvailable measures selection when every
// backend is compatible and available (nil locator reports missing,
// so this uses direct capability resolution through Supports).
func BenchmarkBackendSelectionCompatible(b *testing.B) {
	registry := core.NewRegistry(nil)

	if err := registry.Register(xray.New(), 0); err != nil {
		b.Fatal(err)
	}

	if err := registry.Register(v2ray.New(), 1); err != nil {
		b.Fatal(err)
	}

	if err := registry.Register(singbox.New(), 2); err != nil {
		b.Fatal(err)
	}

	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "bench.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "tcp",
		Security: "tls",
	}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		for _, info := range registry.Backends() {
			backend, _ := registry.Get(info.Name)
			_ = backend.Supports(cfg)
		}
	}
}

// BenchmarkRedactLogText measures the log-redaction hot path.
func BenchmarkRedactLogText(b *testing.B) {
	text := "2026/09/11 [Warning] vless://11111111-1111-1111-1111-111111111111@bench.example.org:443 started"

	secrets := []string{"11111111-1111-1111-1111-111111111111"}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = core.RedactLogText(text, secrets)
	}
}

// BenchmarkGenCache measures generation-cache lookup.
func BenchmarkGenCache(b *testing.B) {
	cache := core.NewGenCache()

	doc := core.RuntimeConfig{FileName: "x.json", Data: []byte("{}")}

	cache.Put("key", 7, doc)

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if _, ok := cache.Get("key", 7); !ok {
			b.Fatal("cache miss")
		}
	}
}

// writeExecutable creates a minimal stand-in file for benchmark
// registries (never executed).
func writeExecutable(path string) error {
	return os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755)
}
