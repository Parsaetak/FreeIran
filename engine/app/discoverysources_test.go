package app

// v0.9.7 tests (§7/§9/§10): the public-source staging ledger —
// persistence, provenance retention and the never-displace rule for
// configured sources.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/discovery"
)

func TestStagingLedgerPersistence(t *testing.T) {
	a := newTestApp(t)

	entries := []stagingEntry{
		{
			URL:        "https://raw.githubusercontent.com/owner/repo/main/sub.txt",
			SourceType: discovery.SourceTypeGitHubTree,
			Trust:      discovery.TrustValidated,
			Provenance: discovery.Provenance{
				SourceID:         "github:abcd1234",
				SourceType:       discovery.SourceTypeGitHubTree,
				SourceURL:        "https://raw.githubusercontent.com/owner/repo/main/sub.txt",
				OriginRepository: "owner/repo",
				OriginPath:       "https://raw.githubusercontent.com/owner/repo/main/sub.txt",
				FirstSeen:        time.Now().UTC().Add(-time.Minute),
				LastSeen:         time.Now().UTC(),
				HTTPStatus:       200,
			},
			Candidates: 3,
			Valid:      3,
		},
	}

	a.publicSources.persistStaged(entries)

	loaded := a.LoadStaged()
	if len(loaded) != 1 {
		t.Fatalf("loaded = %d entries, want 1", len(loaded))
	}

	if loaded[0].URL != entries[0].URL {
		t.Fatalf("url = %s", loaded[0].URL)
	}

	if loaded[0].Provenance.OriginRepository != "owner/repo" {
		t.Fatalf("origin repository lost: %q", loaded[0].Provenance.OriginRepository)
	}

	if loaded[0].Provenance.HTTPStatus != 200 {
		t.Fatalf("http status lost: %d", loaded[0].Provenance.HTTPStatus)
	}

	// Missing file → empty view, never an error.
	_ = os.Remove(filepath.Join(a.layout.Config, "discovered-sources.json"))

	if got := a.LoadStaged(); len(got) != 0 {
		t.Fatalf("missing ledger returned %d entries", len(got))
	}
}

func TestStagingLedgerNeverDisplacesConfiguredSources(t *testing.T) {
	a := newTestApp(t)

	before := len(a.sources)
	if before == 0 {
		t.Skip("no default sources in test app")
	}

	// Simulate a discovery run persisting staged material.
	a.publicSources.persistStaged([]stagingEntry{
		{
			URL:        "https://example.com/found.txt",
			SourceType: discovery.SourceTypeDiscovered,
			Trust:      discovery.TrustDiscovered,
			Provenance: discovery.Provenance{SourceID: "ref:x"},
		},
	})

	// The configured source list is untouched by staging.
	a.mu.RLock()
	after := len(a.sources)
	a.mu.RUnlock()

	if after != before {
		t.Fatalf("configured sources changed: %d → %d", before, after)
	}

	// The staged material lives in its own ledger file.
	if len(a.LoadStaged()) != 1 {
		t.Fatal("staged entry missing")
	}
}
