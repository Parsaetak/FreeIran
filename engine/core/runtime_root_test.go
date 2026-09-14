package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRuntimeRootDefault verifies the historical fallback: without
// SetRuntimeRoot the system temp root is used.
func TestRuntimeRootDefault(t *testing.T) {
	if RuntimeRoot() != os.TempDir() {
		t.Fatalf("RuntimeRoot() = %q, want %q", RuntimeRoot(), os.TempDir())
	}
}

// TestSetRuntimeRoot verifies the composition root can pin the runtime
// directory and NewRunConfig creates per-launch dirs beneath it.
func TestSetRuntimeRootAndRunConfig(t *testing.T) {
	root := t.TempDir()
	SetRuntimeRoot(root)
	defer SetRuntimeRoot("")

	if got := RuntimeRoot(); got != root {
		t.Fatalf("RuntimeRoot() = %q, want %q", got, root)
	}

	rc, err := NewRunConfig("", "xray")
	if err != nil {
		t.Fatalf("NewRunConfig: %v", err)
	}

	dir := rc.Dir()
	if filepath.Dir(dir) != root {
		t.Fatalf("run config dir %q not under runtime root %q", dir, root)
	}

	if base := filepath.Base(dir); len(base) < len("freeiran-") || base[:len("freeiran-")] != "freeiran-" {
		t.Fatalf("run config dir %q lacks freeiran- prefix", dir)
	}

	// Cleanup must remove it (ownership stays with the instance).
	if err := rc.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("run config dir not removed")
	}
}

// TestRemoveStaleRunConfigs verifies the cleanup task: stale freeiran-*
// directories are reclaimed, fresh ones and foreign files stay.
func TestRemoveStaleRunConfigs(t *testing.T) {
	root := t.TempDir()
	SetRuntimeRoot(root)
	defer SetRuntimeRoot("")

	old := time.Now().Add(-12 * time.Hour)

	stale := filepath.Join(root, "freeiran-xray-aaaa")
	fresh := filepath.Join(root, "freeiran-xray-bbbb")
	foreign := filepath.Join(root, "unrelated")

	for _, dir := range []string{stale, fresh, foreign} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	reclaimed, err := RemoveStaleRunConfigs(context.Background(), 6*time.Hour, 256)
	if err != nil {
		t.Fatalf("RemoveStaleRunConfigs: %v", err)
	}

	if reclaimed != int64(len("{}")) {
		t.Fatalf("reclaimed = %d, want %d", reclaimed, len("{}"))
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale run config not removed")
	}

	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh run config must stay")
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("foreign directory must stay")
	}
}
