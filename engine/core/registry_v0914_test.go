package core_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/system"
)

// v0.9.14 — registry projections carry discovery provenance, refresh
// is cache-aware, and an explicit user refresh bypasses the bounded
// freshness windows.

func TestRegistryRefreshSurfacesOriginAndOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is unix-specific")
	}

	dir := t.TempDir()

	script := "#!/bin/sh\necho xray-fake 26.3.27\n"

	if err := os.WriteFile(filepath.Join(dir, system.ExecutableName("xray")), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	registry := core.NewRegistry(system.NewCoreLocator(dir))

	if err := registry.Register(&stubBackend{name: "xray"}, 0); err != nil {
		t.Fatalf("register: %v", err)
	}

	registry.Refresh(context.Background())

	info, ok := registry.Info("xray")
	if !ok {
		t.Fatal("backend missing")
	}

	if info.Status != core.StatusAvailable {
		t.Fatalf("status = %s, want available", info.Status)
	}

	// The fixture lives in the locator's managed directory: managed
	// origin, FreeIran ownership.
	if info.Origin != string(system.OriginManaged) {
		t.Errorf("origin = %q, want %q", info.Origin, system.OriginManaged)
	}

	if info.Ownership != string(system.OwnershipManaged) {
		t.Errorf("ownership = %q, want %q", info.Ownership, system.OwnershipManaged)
	}

	// The public projection exposes the same provenance.
	for _, b := range registry.Backends() {
		if b.Name == "xray" && b.Origin == "" {
			t.Error("Backends() dropped the origin field")
		}
	}
}

func TestRegistryRefreshForceBypassesDiscoveryCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is unix-specific")
	}

	dir := t.TempDir()

	binPath := filepath.Join(dir, system.ExecutableName("v2ray"))

	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho v2ray-fake 5.23.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	registry := core.NewRegistry(system.NewCoreLocator(dir))

	if err := registry.Register(&stubBackend{name: "v2ray"}, 0); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()

	registry.Refresh(ctx)

	if v := registry.Version("v2ray"); v == "" {
		t.Fatal("first refresh discovered no version")
	}

	// Replace the binary with a DIFFERENT version but keep the
	// fingerprint-compatible content size; only a forced refresh (or
	// identity change) may re-probe. We change the CONTENT (identity
	// changes anyway) — the assertion is that RefreshForce reports the
	// new version without waiting for any TTL.
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho v2ray-fake 5.24.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	registry.RefreshForce(ctx)

	if v := registry.Version("v2ray"); v != "v2ray-fake 5.24.0" {
		t.Fatalf("version after forced refresh = %q, want the replaced binary's version", v)
	}
}

func TestRegistryRefreshCacheAwareNoDuplicateWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is unix-specific")
	}

	dir := t.TempDir()

	if err := os.WriteFile(
		filepath.Join(dir, system.ExecutableName("sing-box")),
		[]byte("#!/bin/sh\necho sing-box-fake 1.11.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	registry := core.NewRegistry(system.NewCoreLocator(dir))

	if err := registry.Register(&stubBackend{name: "sing-box"}, 0); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()

	// Successive implicit refreshes hit the shared discovery cache:
	// the file identity is unchanged, so the version stays stable and
	// the registry reports the same candidate without re-spawning
	// probes (the spawn-count guarantee is pinned by the
	// system/execdiscovery tests; this test pins the observable
	// behavior end to end).
	registry.Refresh(ctx)

	first := registry.Version("sing-box")

	for range 3 {
		registry.Refresh(ctx)

		if v := registry.Version("sing-box"); v != first {
			t.Fatalf("cached refresh changed the version: %q -> %q", first, v)
		}
	}

	info, _ := registry.Info("sing-box")

	if info.LastCheck.IsZero() {
		t.Error("refresh must keep LastCheck authoritative")
	}

}
