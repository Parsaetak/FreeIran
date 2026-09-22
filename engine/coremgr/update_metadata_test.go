package coremgr

// v0.9.13 update-metadata regression coverage (§3):
//
//   - CheckForUpdates persists the authoritative upstream snapshot
//     (latest version, release tag, selected platform asset size)
//     into the manifest so the UI can render the real update target
//     and download size across restarts;
//   - manifests written by v0.9.12 (no latest_tag / latest_asset_size)
//     still load correctly — optional fields stay backward-compatible;
//   - a channel change clears the retained snapshot (it was selected
//     under the previous channel).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// releasesListHandler serves the /releases LIST document (the shape
// CheckForUpdates queries: a JSON array) built from the harness's
// single release object. Registered in the test via the harness's
// own mux pattern.
func releasesListHandler(h *installHarness) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		release := h.releaseJSON

		// Wrap the single release object into the one-element array
		// the list endpoint returns.
		var obj map[string]any

		if err := json.Unmarshal(release, &obj); err != nil {
			http.Error(w, "bad release fixture", http.StatusInternalServerError)

			return
		}

		_ = json.NewEncoder(w).Encode([]any{obj})
	}
}

// TestCheckForUpdatesPersistsUpdateMetadata runs a REAL update check
// against the fake release server and proves the manifest retains the
// version/tag/size snapshot — first in memory, then across a manager
// reload (the restart scenario).
func TestCheckForUpdatesPersistsUpdateMetadata(t *testing.T) {
	const coreName = "xray"

	const tag = "v1.2.3"

	h := newInstallHarness(t, coreName, tag)

	// The harness mux serves /releases/latest (the install pipeline);
	// CheckForUpdates consults the /releases LIST document. A wrapper
	// server adds the list route and proxies everything else.
	origMux := h.server.Config.Handler

	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/example/"+coreName+"/releases" {
			releasesListHandler(h)(w, r)

			return
		}

		origMux.ServeHTTP(w, r)
	}))
	t.Cleanup(listSrv.Close)

	src := h.fakeSource(coreName)
	src.ReleaseAPI = listSrv.URL + "/repos/example/" + coreName + "/releases"

	rootDir := t.TempDir()

	mgr, err := New(Options{
		RootDir:              rootDir,
		Sources:              map[CoreName]Source{CoreName(coreName): src},
		HTTPClient:           testClient(),
		DownloadStallTimeout: 1500 * time.Millisecond,
		DownloadMaxRetries:   2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := mgr.CheckForUpdates(ctx, CoreName(coreName))
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}

	if info.AssetSize != int64(len(h.assetBody)) {
		t.Fatalf("UpdateInfo.AssetSize = %d, want %d", info.AssetSize, len(h.assetBody))
	}

	mf, ok := mgr.Info(CoreName(coreName))
	if !ok {
		t.Fatal("manifest missing after update check")
	}

	if mf.LatestKnown != stripV(tag) {
		t.Errorf("manifest LatestKnown = %q, want %q", mf.LatestKnown, stripV(tag))
	}

	if mf.LatestTag != tag {
		t.Errorf("manifest LatestTag = %q, want %q", mf.LatestTag, tag)
	}

	if mf.LatestAssetSize != int64(len(h.assetBody)) {
		t.Errorf("manifest LatestAssetSize = %d, want %d", mf.LatestAssetSize, len(h.assetBody))
	}

	// Reload from the SAME root directory: the snapshot must survive
	// a restart (this is the path the Cores page depends on).
	mgr2, err := New(Options{
		RootDir:    rootDir,
		RuntimeDir: mgr.RuntimeDir(),
		Sources:    map[CoreName]Source{CoreName(coreName): src},
		HTTPClient: testClient(),
	})
	if err != nil {
		t.Fatalf("reload manager: %v", err)
	}

	mf2, ok := mgr2.Info(CoreName(coreName))
	if !ok {
		t.Fatal("manifest missing after reload")
	}

	if mf2.LatestKnown != stripV(tag) || mf2.LatestTag != tag ||
		mf2.LatestAssetSize != int64(len(h.assetBody)) {
		t.Fatalf("snapshot lost across reload: known=%q tag=%q size=%d (want %q/%q/%d)",
			mf2.LatestKnown, mf2.LatestTag, mf2.LatestAssetSize, stripV(tag), tag, len(h.assetBody))
	}
}

// TestManifestBackwardCompatibility writes a manifest WITHOUT the
// v0.9.13 fields (exactly what v0.9.12 persisted) and proves it loads
// unchanged — the new optional fields stay zero, never fabricated.
func TestManifestBackwardCompatibility(t *testing.T) {
	dir := t.TempDir()

	coreDir := filepath.Join(dir, string(CoreXray))

	if err := os.MkdirAll(coreDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// v0.9.12 manifest shape: latest_known present, no latest_tag /
	// latest_asset_size, no failure fields beyond the documented ones.
	legacy := `{
  "name": "xray",
  "state": "update_available",
  "version": "1.2.0",
  "channel": "stable",
  "binary_path": "",
  "checksum_sha256": "abc123",
  "source_url": "https://example.com/asset.zip",
  "release_tag": "v1.2.0",
  "release_date": "2026-08-01T00:00:00Z",
  "release_url": "https://github.com/example/xray/releases/tag/v1.2.0",
  "installed_at": "2026-08-01T00:00:00Z",
  "last_checked": "2026-09-01T00:00:00Z",
  "last_health_check": "2026-08-02T00:00:00Z",
  "last_health_result": {"ok": true, "checked_at": "2026-08-02T00:00:00Z"},
  "latest_known": "1.3.0",
  "updated_at": "2026-09-01T00:00:00Z"
}`

	if err := os.WriteFile(filepath.Join(coreDir, "manifest.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy manifest: %v", err)
	}

	mgr, err := New(Options{RootDir: dir})
	if err != nil {
		t.Fatalf("New with legacy manifest: %v", err)
	}

	mf, ok := mgr.Info(CoreXray)
	if !ok {
		t.Fatal("legacy manifest not loaded")
	}

	if mf.Version != "1.2.0" || mf.State != StateUpdateAvailable || mf.LatestKnown != "1.3.0" {
		t.Fatalf("legacy fields lost: version=%q state=%q latest_known=%q",
			mf.Version, mf.State, mf.LatestKnown)
	}

	if mf.LatestTag != "" || mf.LatestAssetSize != 0 {
		t.Fatalf("new fields must be zero for legacy manifests: tag=%q size=%d",
			mf.LatestTag, mf.LatestAssetSize)
	}

	// A round-trip re-persist must not corrupt anything.
	if err := mgr.SetChannel(CoreXray, ChannelStable); err != nil {
		t.Fatalf("SetChannel on legacy manifest: %v", err)
	}
}

// TestSetChannelClearsUpdateSnapshot proves the retained upstream
// snapshot is invalidated on a channel change: the snapshot was
// selected under the previous channel and must never leak into the
// new one's "update available" rendering.
func TestSetChannelClearsUpdateSnapshot(t *testing.T) {
	dir := t.TempDir()

	mgr, err := New(Options{RootDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Simulate a retained snapshot (as CheckForUpdates would leave it).
	if err := mgr.updateManifest(CoreXray, func(mf *Manifest) {
		mf.State = StateUpdateAvailable
		mf.Version = "1.2.0"
		mf.LatestKnown = "1.3.0"
		mf.LatestTag = "v1.3.0"
		mf.LatestAssetSize = 12345
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	if err := mgr.SetChannel(CoreXray, ChannelPrerelease); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.LatestKnown != "" || mf.LatestTag != "" || mf.LatestAssetSize != 0 {
		t.Fatalf("snapshot not cleared on channel change: known=%q tag=%q size=%d",
			mf.LatestKnown, mf.LatestTag, mf.LatestAssetSize)
	}

	// The cleared snapshot must be what is persisted.
	raw, err := os.ReadFile(mgr.ManifestPath(CoreXray))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var onDisk Manifest

	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}

	if onDisk.LatestKnown != "" || onDisk.LatestTag != "" || onDisk.LatestAssetSize != 0 {
		t.Fatalf("cleared snapshot not persisted: %v", onDisk)
	}
}
