package coremgr

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/system"
)

// ---------------------------------------------------------------------------
// v0.9.14 reuse-first install decisions
//
// The harness serves a real release; fakecore stands in for the core
// binary. External installations are simulated through the locator's
// injectable PATH lookup (the same discovery authority production
// uses), so every reuse decision runs against the real discovery,
// probe, validation and smoke pipeline.
// ---------------------------------------------------------------------------

// copyFakeExternal places a working fakecore copy at an "external"
// location and returns its path.
func copyFakeExternal(t *testing.T, dir, coreName string) string {
	t.Helper()

	buildFakeCore()
	if fakeCoreErr != nil {
		t.Skipf("fakecore unavailable: %v", fakeCoreErr)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	extPath := filepath.Join(dir, executableName(coreName))

	raw, err := os.ReadFile(fakeCorePath)
	if err != nil {
		t.Fatalf("read fakecore: %v", err)
	}

	if err := os.WriteFile(extPath, raw, 0o755); err != nil {
		t.Fatalf("write external fakecore: %v", err)
	}

	return extPath
}

// newLocatorWithExternal builds a locator whose PATH resolves coreName
// to the given external path (origin "path", ownership "external").
func newLocatorWithExternal(t *testing.T, coreName, extPath string) *system.CoreLocator {
	t.Helper()

	loc := system.NewCoreLocator(t.TempDir())

	loc.SetLookPath(func(name string) (string, error) {
		if name == coreName {
			return extPath, nil
		}

		return "", fs.ErrNotExist
	})

	return loc
}

func TestInstallCurrentExternalNoDownload(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	mgr := newTestManager(t, h, coreName)

	extPath := copyFakeExternal(t, filepath.Join(t.TempDir(), "tooling"), coreName)
	mgr.SetLocator(newLocatorWithExternal(t, coreName, extPath))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The system already has exactly the stable release: adopt it,
	// never download.
	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if hits := h.snapshot().assetHits; hits != 0 {
		t.Fatalf("asset downloads = %d, want 0 (external core reused)", hits)
	}

	mf, ok := mgr.Info(CoreXray)
	if !ok {
		t.Fatal("manifest missing after adoption")
	}

	if mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}

	if mf.Ownership != string(system.OwnershipExternal) {
		t.Errorf("ownership = %q, want external", mf.Ownership)
	}

	if mf.Origin != string(system.OriginPath) {
		t.Errorf("origin = %q, want path", mf.Origin)
	}

	if mf.ExternalPath != extPath || mf.BinaryPath != extPath {
		t.Errorf("external reference = %q/%q, want %q", mf.ExternalPath, mf.BinaryPath, extPath)
	}

	if mf.LastDecision != string(DecisionSystemCurrent) {
		t.Errorf("last_decision = %q, want %q", mf.LastDecision, DecisionSystemCurrent)
	}

	if _, err := os.Stat(extPath); err != nil {
		t.Error("external binary was modified by adoption")
	}
}

func TestInstallOlderExternalReusedUpdateSurfaced(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v1.0.0")

	mgr := newTestManager(t, h, coreName)

	extPath := copyFakeExternal(t, filepath.Join(t.TempDir(), "tooling"), coreName)
	mgr.SetLocator(newLocatorWithExternal(t, coreName, extPath))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// A working older external binary beats an unnecessary download.
	if hits := h.snapshot().assetHits; hits != 0 {
		t.Fatalf("asset downloads = %d, want 0 (older working core retained)", hits)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.State != StateUpdateAvailable {
		t.Errorf("state = %s, want update_available", mf.State)
	}

	if mf.LatestKnown != "2.0.0" {
		t.Errorf("latest_known = %q, want 2.0.0 (update surfaced)", mf.LatestKnown)
	}

	if mf.Ownership != string(system.OwnershipExternal) {
		t.Errorf("ownership = %q, want external", mf.Ownership)
	}

	if _, err := os.Stat(extPath); err != nil {
		t.Error("external binary was modified")
	}
}

func TestInstallNewerExternalNeverDowngraded(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v3.0.0")

	mgr := newTestManager(t, h, coreName)

	extPath := copyFakeExternal(t, filepath.Join(t.TempDir(), "tooling"), coreName)
	mgr.SetLocator(newLocatorWithExternal(t, coreName, extPath))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if hits := h.snapshot().assetHits; hits != 0 {
		t.Fatalf("asset downloads = %d, want 0 (never downgrade)", hits)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}

	if mf.StatusNote == "" {
		t.Error("status_note empty; the newer-than-stable state must be surfaced honestly")
	}

	if mf.LastDecision != string(DecisionSystemNewer) {
		t.Errorf("last_decision = %q, want %q", mf.LastDecision, DecisionSystemNewer)
	}
}

func TestInstallInvalidExternalFallsThroughToDownload(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v2.0.0")
	t.Setenv("FAKECORE_FAIL_FAST", "1")

	mgr := newTestManager(t, h, coreName)

	extPath := copyFakeExternal(t, filepath.Join(t.TempDir(), "tooling"), coreName)
	mgr.SetLocator(newLocatorWithExternal(t, coreName, extPath))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The external binary exists but fails validation: the pipeline
	// falls through to the official download (which also fails its
	// smoke — the same fake binary — so the install ends Broken, but
	// the decisive assertion is that the downloader RAN).
	_ = mgr.Install(ctx, CoreXray)

	if hits := h.snapshot().assetHits; hits != 1 {
		t.Fatalf("asset downloads = %d, want 1 (invalid external → acquisition)", hits)
	}

	if _, err := os.Stat(extPath); err != nil {
		t.Error("invalid external binary must still not be modified or deleted")
	}
}

func TestUninstallPreservesExternalBinary(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	mgr := newTestManager(t, h, coreName)

	extPath := copyFakeExternal(t, filepath.Join(t.TempDir(), "tooling"), coreName)
	mgr.SetLocator(newLocatorWithExternal(t, coreName, extPath))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if err := mgr.Remove(ctx, CoreXray); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Stat(extPath); err != nil {
		t.Fatal("uninstall deleted the EXTERNAL binary — user files must never be touched")
	}
}

func TestUpdateFromExternalPreservesExternalFile(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v1.0.0")

	mgr := newTestManager(t, h, coreName)

	extPath := copyFakeExternal(t, filepath.Join(t.TempDir(), "tooling"), coreName)
	mgr.SetLocator(newLocatorWithExternal(t, coreName, extPath))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Adopt the older external core first.
	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("adopt Install: %v", err)
	}

	before, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatal(err)
	}

	// Explicit update: the managed v2 replaces the REFERENCE, the
	// external file stays byte for byte.
	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	if err := mgr.Acquire(ctx, CoreXray); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.Ownership != string(system.OwnershipManaged) {
		t.Errorf("ownership = %q, want managed after explicit update", mf.Ownership)
	}

	if mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}

	after, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatal("external binary vanished after explicit update")
	}

	if string(before) != string(after) {
		t.Error("external binary changed during a managed update")
	}
}

func TestRollbackPreservesExternalBinary(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v1.0.0")

	mgr := newTestManager(t, h, coreName)

	extPath := copyFakeExternal(t, filepath.Join(t.TempDir(), "tooling"), coreName)
	mgr.SetLocator(newLocatorWithExternal(t, coreName, extPath))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("adopt Install: %v", err)
	}

	// Update to managed v2; the external file remains on disk.
	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	if err := mgr.Acquire(ctx, CoreXray); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Rollback has no MANAGED previous binary to restore (the previous
	// active binary was an external reference): the operation must
	// fail honestly — never by deleting or renaming the external file.
	if err := mgr.Rollback(ctx, CoreXray); !errors.Is(err, ErrNoRollbackTarget) {
		t.Errorf("Rollback error = %v, want ErrNoRollbackTarget", err)
	}

	if _, err := os.Stat(extPath); err != nil {
		t.Fatal("external binary vanished during rollback")
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.Ownership != string(system.OwnershipManaged) || mf.BinaryPath == extPath {
		t.Errorf("ownership/reference disturbed by rollback: %q %q", mf.Ownership, mf.BinaryPath)
	}
}

// TestInstallOlderManagedReusedLocallyAndEnriched pins the true
// local-first semantics for a healthy MANAGED core older than the
// fresh release: Install answers from LOCAL evidence (the network is
// never consulted on the critical path, nothing is downloaded), the
// working runtime is kept byte for byte, and the detached enrichment
// afterwards refines the manifest against the fresh target — the
// runtime stays usable while the update becomes visible. The explicit
// update transaction itself is covered by
// TestInstallSmokeTestFailureKeepsPreviousCore (via Acquire).
func TestInstallOlderManagedReusedLocallyAndEnriched(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v8.0.0")

	t.Setenv("FAKECORE_VERSION", "v8.0.0")

	mgr := newTestManager(t, h, coreName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// First install: healthy managed core at v8.0.0.
	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	before, _ := mgr.Info(CoreXray)
	beforeSum, _ := fileSHA256(before.BinaryPath)

	// A newer release is published, then Install runs again.
	firstSnap := h.snapshot()

	h.setRelease("v8.1.0")

	// The reuse answer must be bounded by LOCAL work: a regression to
	// the inline enrichment wait (the full reuseResolveBudget) would
	// overshoot this bound deterministically.
	start := time.Now()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("local reuse took %s; the reuse answer must not wait on the network", elapsed)
	}

	if second := h.snapshot(); second.assetHits != firstSnap.assetHits {
		t.Fatalf("asset downloads grew by %d, want 0 (older healthy managed core kept)",
			second.assetHits-firstSnap.assetHits)
	}

	after, _ := mgr.Info(CoreXray)

	afterSum, err := fileSHA256(after.BinaryPath)
	if err != nil {
		t.Fatalf("active binary vanished: %v", err)
	}

	if afterSum != beforeSum {
		t.Fatal("active binary changed during local reuse")
	}

	// Enrichment refinement: the fresh target (v8.1.0) marks the update
	// available while the runtime stays healthy and usable. Waiting on
	// the manager's enrichment group is the deterministic completion
	// signal — no polling.
	mgr.waitEnrichments()

	mf, _ := mgr.Info(CoreXray)

	if mf.State != StateUpdateAvailable {
		t.Errorf("state = %s, want update_available (fresh target surfaced by the enrichment)", mf.State)
	}

	if mf.LastDecision != string(DecisionManagedHealthy) {
		t.Errorf("last_decision = %q, want managed-healthy (kept, update surfaced)", mf.LastDecision)
	}

	if mf.LatestKnown != "8.1.0" {
		t.Errorf("latest_known = %q, want 8.1.0 (release metadata refined)", mf.LatestKnown)
	}
}

func TestCurrentManagedInstallIsNoOp(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	mgr := newTestManager(t, h, coreName)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	first := h.snapshot()

	if first.assetHits != 1 {
		t.Fatalf("first install asset downloads = %d, want 1", first.assetHits)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.BinarySize == 0 || mf.BinaryModTime.IsZero() {
		t.Fatal("managed install must record the binary file identity")
	}

	// Second Install on the already-current core: a true no-op on the
	// critical path (local evidence only — zero inline network work).
	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	second := h.snapshot()

	if second.assetHits != first.assetHits {
		t.Fatalf("asset downloads = %d, want %d (no re-download for a current core)", second.assetHits, first.assetHits)
	}

	// Exactly ONE resolve per install call, now delivered by the
	// DETACHED enrichment that follows a local reuse: wait for it
	// to settle, then pin the count — two calls, two conditional
	// API requests, zero asset downloads.
	mgr.waitEnrichments()

	second = h.snapshot()

	if second.apiHits != first.apiHits+1 {
		t.Fatalf("API hits = %d, want %d (one enrichment resolve per reuse)", second.apiHits, first.apiHits+1)
	}

	if mf, _ = mgr.Info(CoreXray); mf.LastDecision != string(DecisionManagedCurrent) {
		t.Errorf("last_decision = %q, want %q", mf.LastDecision, DecisionManagedCurrent)
	}
}

func TestStagedCompleteArtifactReused(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	mgr := newTestManager(t, h, coreName)

	// Simulate an interrupted install that completed the DOWNLOAD but
	// died before unpacking: the staged archive is complete and valid.
	staging := mgr.StagingDir(CoreXray)

	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}

	assetPath := filepath.Join(staging, h.assetName)

	if err := os.WriteFile(assetPath, h.assetBody, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if hits := h.snapshot().assetHits; hits != 0 {
		t.Fatalf("asset downloads = %d, want 0 (complete staged artifact reused)", hits)
	}

	if mf, _ := mgr.Info(CoreXray); mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}
}

func TestStagedCorruptArtifactRejectedAndRedownloaded(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	mgr := newTestManager(t, h, coreName)

	staging := mgr.StagingDir(CoreXray)

	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}

	// Same size as the real asset, corrupt content: the digest check
	// must reject exactly this artifact.
	corrupt := append([]byte(nil), h.assetBody...)
	corrupt[len(corrupt)/2] ^= 0xFF

	assetPath := filepath.Join(staging, h.assetName)

	if err := os.WriteFile(assetPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if hits := h.snapshot().assetHits; hits != 1 {
		t.Fatalf("asset downloads = %d, want 1 (corrupt staged artifact redownloaded)", hits)
	}

	if mf, _ := mgr.Info(CoreXray); mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}
}

func TestReleaseMetadataConcurrentRequestsDeduplicated(t *testing.T) {
	const coreName = "xray"

	h := newInstallHarness(t, coreName, "v6.0.0")

	t.Setenv("FAKECORE_VERSION", "v6.0.0")

	mgr := newTestManager(t, h, coreName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Six concurrent callers request the SAME release endpoint: ONE
	// network request must serve them all (v0.9.14 in-flight dedup).
	const callers = 6

	var wg sync.WaitGroup

	errs := make([]error, callers)

	for i := range callers {
		wg.Add(1)

		go func(slot int) {
			defer wg.Done()

			_, errs[slot] = mgr.CheckForUpdates(ctx, CoreXray)
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("CheckForUpdates[%d]: %v", i, err)
		}
	}

	if hits := h.snapshot().apiHits; hits != 1 {
		t.Fatalf("API hits = %d, want 1 (concurrent duplicate requests deduplicated)", hits)
	}
}
