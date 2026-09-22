package core_test

// v0.9.13 regression coverage for the CONCURRENT registry refresh
// (§4 performance audit): Refresh spawns one discovery goroutine per
// registered backend (bounded at three). The test exercises exactly
// that path — real executables on disk, real version probes — while
// concurrent readers run, so the race detector sees every interleaving
// the desktop app can produce. Result integrity and priority ordering
// must be identical to the previous serial implementation.
//
// The version-probe executables are unix-only shell scripts (the same
// fixture pattern system_test.go uses); on Windows the status update
// is still exercised, without the probe binaries.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/system"
)

func TestRegistryRefreshConcurrentUpdatesAllBackends(t *testing.T) {
	dir := t.TempDir()

	names := []string{"xray", "v2ray", "sing-box"}

	if runtime.GOOS != "windows" {
		for _, name := range names {
			path := filepath.Join(dir, system.ExecutableName(name))
			script := fmt.Sprintf("#!/bin/sh\necho %s-fake 26.3.27\n", name)

			if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
	}

	registry := core.NewRegistry(system.NewCoreLocator(dir))

	for i, name := range names {
		if err := registry.Register(&stubBackend{name: name}, i); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	// Concurrent readers while Refresh runs: the UI can read
	// Backends()/Info() at any point during discovery.
	var wg sync.WaitGroup

	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for {
				select {
				case <-stop:
					return
				default:
					_ = registry.Backends()

					_, _ = registry.Info("xray")
				}
			}
		}()
	}

	// Several refreshes in a row: the second and third hit the
	// update-in-place path with all entries already present.
	for i := 0; i < 3; i++ {
		registry.Refresh(context.Background())
	}

	close(stop)
	wg.Wait()

	backends := registry.Backends()

	if len(backends) != len(names) {
		t.Fatalf("backends = %d, want %d", len(backends), len(names))
	}

	// Deterministic priority order must survive concurrent refresh.
	for i, name := range names {
		if backends[i].Name != name {
			t.Fatalf("backends[%d].Name = %q, want %q", i, backends[i].Name, name)
		}
	}

	if runtime.GOOS != "windows" {
		for _, backend := range backends {
			if backend.Status != core.StatusAvailable {
				t.Fatalf("%s status = %s, want available", backend.Name, backend.Status)
			}

			if backend.Version == "" {
				t.Fatalf("%s has no version after refresh", backend.Name)
			}
		}
	}
}
