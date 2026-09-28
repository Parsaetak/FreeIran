package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/coremgr"
	"github.com/Parsaetak/FreeIran/system"
)

// TestManagedCoreBinDirsAreDiscoveryInputs is the regression test for
// the v0.8 integration gap: a core installed by the Managed Core
// Manager lives in <cores>/<core>/bin/<core>.exe, but the registry
// locator only searched <cores>/ — so installs were invisible to
// Connect/Backends/Tester. The locator must now include the managed
// bin directories, and a binary dropped into one must be discovered.
//
// v0.11.2 split: the test iterates over TWO core sets.
//
//   - coremgr.AllCores: every managed core's BinaryPath is on disk and
//     discoverable by the locator (Mihomo included — it is
//     coremgr-managed even though its connection-engine adapter is
//     not yet wired; the spec forbids a large refactor).
//   - connectionEngineBackends: the subset that ALSO has a registered
//     protocol-core adapter (xray, v2ray, sing-box). Only these are
//     expected to show up in the registry's Backends() — advertising
//     Mihomo as a runnable protocol-core backend before its adapter
//     exists would violate "Only advertise actual compatibility".
func TestManagedCoreBinDirsAreDiscoveryInputs(t *testing.T) {
	application := newTestApp(t)

	mgr := application.CoreManager()
	if mgr == nil {
		t.Fatal("core manager is nil after New (must boot eagerly)")
	}

	// Simulate a managed install: the exact path BinaryPath reports
	// must be discoverable by the app's locator — for EVERY managed
	// core, including Mihomo (whose connection-engine adapter is not
	// yet wired but whose install/health/UI path is).
	for _, name := range coremgr.AllCores {
		binPath := mgr.BinaryPath(name)

		if err := os.MkdirAll(filepath.Dir(binPath), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(binPath), err)
		}

		if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatalf("write %s: %v", binPath, err)
		}
	}

	extra := make([]string, 0, len(coremgr.AllCores))
	for _, name := range coremgr.AllCores {
		extra = append(extra, mgr.BinDir(name))
	}

	locator := system.NewCoreLocator(application.layout.Cores, extra...)

	// Sanity: the locator must discover EVERY managed core binary
	// by its on-disk path. This is the regression the v0.8 test
	// originally defended.
	for _, name := range coremgr.AllCores {
		bin, err := locator.Discover(t.Context(), string(name))
		if err != nil {
			t.Errorf("discover %s: %v", name, err)
			continue
		}
		if bin.Path == "" {
			t.Errorf("discover %s returned empty path", name)
		}
	}

	// The app's own registry (built on the same bin dirs) must see
	// the backends that have a registered protocol-core adapter —
	// xray/v2ray/sing-box. Mihomo is coremgr-managed (install +
	// health + UI surface) but its connection-engine adapter is not
	// wired in v0.11.2 (the spec forbids the large refactor), so it
	// MUST NOT be advertised as a runnable backend here.
	application.RefreshCores()

	backends := application.coreRegistry.Backends()
	available := map[string]bool{}

	for _, b := range backends {
		available[b.Name] = b.Status == "available"
	}

	// The set of registered protocol-core backends — the ONLY cores
	// the connection engine may route traffic through. Adding a new
	// managed core to coremgr.AllCores does NOT add it here; that
	// requires a registered adapter in engine/core/<name>/ and a
	// Register call in engine/app/{app,v6_services}.go.
	connectionEngineBackends := []coremgr.CoreName{
		coremgr.CoreXray, coremgr.CoreV2Ray, coremgr.CoreSingBox,
	}

	for _, name := range connectionEngineBackends {
		if !available[string(name)] {
			t.Errorf("backend %s not available after managed install simulation", name)
		}
	}

	// Defensive: Mihomo must NOT be advertised as a runnable
	// protocol-core backend in v0.11.2. If a future change wires the
	// adapter, this assertion flips to "available" — and this test
	// becomes the gate that documents the capability surface
	// honestly. (See the spec: "Only advertise actual compatibility.")
	if available[string(coremgr.CoreMihomo)] {
		t.Errorf("mihomo is advertised as a runnable backend, but its protocol-core adapter is not wired in v0.11.2; refusing to claim a capability that is not actually present")
	}
}
