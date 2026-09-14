package coremgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanStaleStaging verifies stale staging trees and failed-
// download artifacts are reclaimed by age while live core metadata
// (bin/, manifest, rollback) is never touched.
func TestCleanStaleStaging(t *testing.T) {
	root := t.TempDir()

	m, err := New(Options{RootDir: root, Logger: nil})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	old := time.Now().Add(-14 * 24 * time.Hour)

	// Stale staging content (old) + fresh staging content.
	staleStaging := filepath.Join(m.StagingDir(CoreXray), "unpacked")
	freshStaging := filepath.Join(m.StagingDir(CoreV2Ray), "unpacked")

	for _, dir := range []string{staleStaging, freshStaging} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir staging: %v", err)
		}

		if err := os.WriteFile(filepath.Join(dir, "asset.zip"), []byte("payload"), 0o600); err != nil {
			t.Fatalf("write asset: %v", err)
		}
	}

	if err := os.Chtimes(staleStaging, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Live metadata that must survive.
	bin := m.BinDir(CoreXray)
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	if err := os.WriteFile(filepath.Join(bin, "xray"), []byte("binary"), 0o700); err != nil {
		t.Fatalf("write bin: %v", err)
	}

	reclaimed, err := m.CleanStaleStaging(context.Background(), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("CleanStaleStaging: %v", err)
	}

	if reclaimed != int64(len("payload")) {
		t.Fatalf("reclaimed = %d, want %d", reclaimed, len("payload"))
	}

	if _, err := os.Stat(staleStaging); !os.IsNotExist(err) {
		t.Fatal("stale staging tree not removed")
	}

	if _, err := os.Stat(freshStaging); err != nil {
		t.Fatal("fresh staging tree must be kept")
	}

	if _, err := os.Stat(filepath.Join(bin, "xray")); err != nil {
		t.Fatal("live bin must never be touched")
	}
}
