// quickconnect_v099_test.go — v0.9.9 Quick Connect execution-quality
// regression coverage (§9):
//
//   - collectCandidateRecords performs ONE bounded pass: every stored
//     configuration is returned exactly once with its stable ID, and
//     the scan bound is honoured (the pre-0.9.9 implementation scanned
//     the store twice and its own pass was unbounded);
//   - freshTestShortlist refreshes ALL stale shortlist entries through
//     the fixed worker pool and persists the fresh evidence;
//   - route-trust and cooldown policies are preserved (the parallel
//     path reuses the same filters).
package app

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func newQuickConnectTestApp(t *testing.T) *App {
	t.Helper()

	application, err := New(Options{
		SkipConnectVerification: true,
		BaseDir:                 filepath.Join(t.TempDir(), "freeiran"),
		RefreshInterval:         time.Hour,
		RunIngestionOnStart:     false,
		SkipDefaultSources:      true,
	})
	if err != nil {
		t.Fatalf("app.New() = %v", err)
	}

	t.Cleanup(application.Shutdown)

	return application
}

// trustedConfig builds a user-trusted configuration with a stable ID.
func trustedConfig(name string, port int) config.Config {
	return config.Config{
		Type:        config.TypeVLESS,
		Name:        name,
		Address:     fmt.Sprintf("%s.example.org", name),
		Port:        port,
		UUID:        "11111111-1111-1111-1111-111111111111",
		Network:     "tcp",
		Security:    "tls",
		SourceTrust: config.SourceTrustUser,
	}
}

// TestCollectCandidateRecordsReturnsEveryRecordOnce proves the single
// bounded pass returns every stored configuration exactly once with
// stable IDs.
func TestCollectCandidateRecordsReturnsEveryRecordOnce(t *testing.T) {
	application := newQuickConnectTestApp(t)

	const n = 25

	for i := 0; i < n; i++ {
		storeConfig(t, application, trustedConfig(fmt.Sprintf("cfg-%02d", i), 10000+i))
	}

	records := application.collectCandidateRecords(application.ctx)

	if len(records) != n {
		t.Fatalf("collectCandidateRecords returned %d records, want %d", len(records), n)
	}

	seen := map[string]bool{}

	for _, rec := range records {
		if rec.cfg.ID == "" {
			t.Fatal("record with empty stable ID")
		}

		if seen[rec.cfg.ID] {
			t.Fatalf("duplicate record for ID %s — the pass is not single", rec.cfg.ID)
		}

		seen[rec.cfg.ID] = true
	}
}

// TestCollectCandidateRecordsScanBound proves the candidateScanLimit
// bound (the pre-0.9.9 pass was UNBOUNDED).
func TestCollectCandidateRecordsScanBound(t *testing.T) {
	application := newQuickConnectTestApp(t)

	// Store more records than the bound; iteration must stop at the
	// bound (never at the full store size).
	for i := 0; i < candidateScanLimit+50; i++ {
		cfg := trustedConfig(fmt.Sprintf("bulk-%d", i), 20000+i%20000)
		cfg.Normalize()
		cfg.SetID()

		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}

		if err := application.store.Upsert(cfg.Fingerprint(), raw); err != nil {
			t.Fatal(err)
		}
	}

	records := application.collectCandidateRecords(application.ctx)

	if len(records) != candidateScanLimit {
		t.Fatalf("scan returned %d records, want the bound %d", len(records), candidateScanLimit)
	}
}

// TestFreshTestShortlistRefreshesAllStale proves the (now parallel)
// fresh-testing phase keeps its semantic contract: every stale
// shortlist entry is re-measured through the SAME tester and the
// fresh evidence is persisted.
func TestFreshTestShortlistRefreshesAllStale(t *testing.T) {
	application := newQuickConnectTestApp(t)

	var records []qcRecord

	for i := 0; i < 5; i++ {
		cfg := trustedConfig(fmt.Sprintf("stale-%d", i), 30000+i)

		// Stale evidence: measured long ago.
		cfg.TestedAt = time.Now().Add(-48 * time.Hour).UnixMilli()

		id := storeConfig(t, application, cfg)

		records = append(records, qcRecord{cfg: storedConfigByID(t, application, id)})
	}

	tested := application.freshTestShortlist(application.ctx, records)

	if tested != len(records) {
		t.Fatalf("freshTestShortlist refreshed %d of %d records", tested, len(records))
	}

	for _, rec := range records {
		fresh := storedConfigByID(t, application, rec.cfg.ID)

		if fresh.TestedAt == 0 || fresh.TestedAt <= rec.cfg.TestedAt {
			t.Fatalf("candidate %s was not refreshed (TestedAt=%d)", rec.cfg.ID, fresh.TestedAt)
		}
	}
}

// TestFreshTestShortlistSkipsFresh proves the freshness gate: records
// with fresh evidence are NOT re-measured.
func TestFreshTestShortlistSkipsFresh(t *testing.T) {
	application := newQuickConnectTestApp(t)

	cfg := trustedConfig("fresh-cfg", 31000)
	cfg.TestedAt = time.Now().UnixMilli() // fresh

	id := storeConfig(t, application, cfg)

	records := []qcRecord{{cfg: storedConfigByID(t, application, id)}}

	if tested := application.freshTestShortlist(application.ctx, records); tested != 0 {
		t.Fatalf("fresh evidence was re-tested (%d)", tested)
	}
}

// storedConfigByID reloads a stored configuration record.
func storedConfigByID(t *testing.T, application *App, id string) config.Config {
	t.Helper()

	raw, err := application.store.Get(id)
	if err != nil || raw == nil {
		t.Fatalf("load config %s: %v", id, err)
	}

	var cfg config.Config

	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal %s: %v", id, err)
	}

	return cfg
}
