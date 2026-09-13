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
func TestManagedCoreBinDirsAreDiscoveryInputs(t *testing.T) {
	application := newTestApp(t)

	mgr := application.CoreManager()
	if mgr == nil {
		t.Fatal("core manager is nil after New (must boot eagerly)")
	}

	// Simulate a managed install: the exact path BinaryPath reports
	// must be discoverable by the app's locator.
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

	for _, name := range coremgr.AllCores {
		bin, err := locator.Discover(t.Context(), string(name))
		if err != nil {
			t.Fatalf("discover %s: %v", name, err)
		}

		if bin.Path == "" {
			t.Errorf("discover %s returned empty path", name)
		}
	}

	// The app's own registry (built on the same bin dirs) must see
	// them after a refresh — the same semantics the CoreService
	// applies after install/update/remove.
	application.RefreshCores()

	backends := application.coreRegistry.Backends()
	available := map[string]bool{}

	for _, b := range backends {
		available[b.Name] = b.Status == "available"
	}

	for _, name := range coremgr.AllCores {
		if !available[string(name)] {
			t.Errorf("backend %s not available after managed install simulation", name)
		}
	}
}
