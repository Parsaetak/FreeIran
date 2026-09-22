package system

// v0.9.13 hot-path benchmark (§4 performance audit): CoreLocator.
// Discover is the per-core executable probe behind every registry
// refresh. The audit (v0.9.13) verified refreshes are event-driven
// (boot, post-install, explicit user action) — never polled — so the
// benchmark pins the per-call cost instead of justifying a cache:
// the miss path walks the candidate directories plus PATH, the hit
// path is a directory probe plus one version-probe process spawn.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkCoreLocatorDiscoverMiss(b *testing.B) {
	dir := b.TempDir()

	locator := NewCoreLocator(dir)

	ctx := context.Background()

	b.ReportAllocs()

	for b.Loop() {
		if _, err := locator.Discover(ctx, "xray"); err == nil {
			b.Fatal("unexpected discovery in empty directory")
		}
	}
}

func BenchmarkCoreLocatorDiscoverHit(b *testing.B) {
	dir := b.TempDir()

	bin := filepath.Join(dir, executableName("fakecore"))

	// A copy of the test binary stands in for a core executable: the
	// version probe spawns a process whose output never parses to a
	// version — the measured cost is the real probe cost, only the
	// fixture differs from a production core.
	if exe, err := os.Executable(); err == nil {
		raw, rerr := os.ReadFile(exe)
		if rerr == nil {
			if werr := os.WriteFile(bin, raw, 0o700); werr == nil {
				b.Cleanup(func() { _ = os.Remove(bin) })
			}
		}
	}

	locator := NewCoreLocator(dir)

	ctx := context.Background()

	b.ReportAllocs()

	for b.Loop() {
		binary, err := locator.Discover(ctx, "fakecore")
		if err != nil {
			b.Fatalf("discover: %v", err)
		}

		if binary.Path != bin {
			b.Fatalf("discovered %q, want %q", binary.Path, bin)
		}
	}
}
