package appupdate

import (
	"context"

	"github.com/Parsaetak/FreeIran/internal/httpx"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const feedJSON = `{
  "tag_name": "v0.10.0",
  "html_url": "https://github.com/Parsaetak/FreeIran/releases/tag/v0.10.0",
  "prerelease": false,
  "published_at": "2026-09-01T00:00:00Z",
  "assets": [
    {"name": "FreeIran-v0.10.0-windows-amd64.zip",
     "browser_download_url": "https://example.test/FreeIran-v0.10.0-windows-amd64.zip",
     "size": 42424242},
    {"name": "FreeIran-v0.10.0-windows-amd64.zip.sha256",
     "browser_download_url": "https://example.test/FreeIran-v0.10.0-windows-amd64.zip.sha256",
     "size": 110},
    {"name": "FreeIran-v0.10.0-linux-amd64.zip",
     "browser_download_url": "https://example.test/FreeIran-v0.10.0-linux-amd64.zip",
     "size": 40404040}
  ]
}`

// appUpdateTestClient is the real httpx client with a fast retry
// policy, so the tests exercise the production network path.
func appUpdateTestClient(t *testing.T) *httpx.Client {
	t.Helper()

	c := httpx.NewClient(httpx.Policy{
		RequestTimeout: 5 * time.Second,
		MaxRetries:     1,
		BackoffBase:    2 * time.Millisecond,
		BackoffMax:     5 * time.Millisecond,
	})

	t.Cleanup(c.Close)

	return c
}

func feedServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)

			_, _ = w.Write([]byte(body))
		}))

	t.Cleanup(server.Close)

	return server
}

func TestCheckSelectsPlatformAsset(t *testing.T) {
	server := feedServer(t, feedJSON, http.StatusOK)

	info, err := Check(context.Background(), appUpdateTestClient(t), server.URL, "0.9.4", "windows")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if !info.UpdateAvailable {
		t.Fatal("0.9.4 → 0.10.0 must be an update")
	}

	if info.LatestVersion != "0.10.0" || info.ReleaseTag != "v0.10.0" {
		t.Fatalf("unexpected versions: %+v", info)
	}

	if info.Asset == nil {
		t.Fatal("windows asset not selected")
	}

	if info.Asset.Name != "FreeIran-v0.10.0-windows-amd64.zip" {
		t.Fatalf("wrong asset: %s", info.Asset.Name)
	}

	// §20: the checksum sidecar URL rides along with the asset so the
	// downloader can verify before staging.
	if info.Asset.ChecksumURL != info.Asset.URL+".sha256" {
		t.Fatalf("checksum URL wrong: %s", info.Asset.ChecksumURL)
	}
}

func TestCheckNoUpdateWhenCurrent(t *testing.T) {
	server := feedServer(t, feedJSON, http.StatusOK)

	info, err := Check(context.Background(), appUpdateTestClient(t), server.URL, "0.10.0", "windows")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if info.UpdateAvailable {
		t.Fatal("same version reported as update")
	}

	// No update → no download surface at all (least privilege).
	if info.Asset != nil {
		t.Fatalf("asset surfaced without an update: %+v", info.Asset)
	}
}

func TestCheckFeedErrorIsHonest(t *testing.T) {
	server := feedServer(t, `{"message": "rate limited"}`, http.StatusForbidden)

	if _, err := Check(context.Background(), appUpdateTestClient(t), server.URL, "0.9.4", "windows"); err == nil {
		t.Fatal("HTTP 403 accepted as a valid check")
	}
}

func TestCheckLinuxGetsNoWindowsAsset(t *testing.T) {
	server := feedServer(t, feedJSON, http.StatusOK)

	info, err := Check(context.Background(), appUpdateTestClient(t), server.URL, "0.9.4", "linux")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if !info.UpdateAvailable {
		t.Fatal("update availability lost")
	}

	if info.Asset != nil && info.Asset.Name != "FreeIran-v0.10.0-linux-amd64.zip" {
		t.Fatalf("wrong asset for linux: %s", info.Asset.Name)
	}
}

func TestIsNewerSemver(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"0.9.4", "0.9.4", false},
		{"0.9.5", "0.9.4", true},
		{"0.10.0", "0.9.4", true},
		{"1.0.0", "0.9.9", true},
		{"0.9.3", "0.9.4", false},
		{"0.9", "0.9.4", false},
		{"v0.10.0", "0.9.4", true},
		// Pre-releases never outrank their release.
		{"0.9.4-rc.1", "0.9.4", false},
		{"0.9.4", "0.9.4-rc.1", true},
		// CI-stamped builds ("-ci" suffix) are pre-release-like: the
		// same-version release outranks them (and a NEWER -ci build
		// still outranks an older release by its numeric triple).
		{"0.9.4-ci", "0.9.4", false},
		{"0.9.5-ci", "0.9.4", true},
	}

	for _, tc := range cases {
		if got := IsNewer(tc.candidate, tc.current); got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

func TestParseChecksumFile(t *testing.T) {
	content := "aabbccdd00112233aabbccdd00112233aabbccdd00112233aabbccdd00112233  FreeIran-v0.9.4-windows-amd64.zip\n" +
		"1122334455667788112233445566778811223344556677881122334455667788  FreeIran-v0.9.4-linux-amd64.zip\n"

	got, err := ParseChecksumFile(content, "FreeIran-v0.9.4-windows-amd64.zip")
	if err != nil {
		t.Fatalf("ParseChecksumFile: %v", err)
	}

	if got != "aabbccdd00112233aabbccdd00112233aabbccdd00112233aabbccdd00112233" {
		t.Fatalf("wrong digest: %s", got)
	}

	if _, err := ParseChecksumFile(content, "FreeIran-unknown.zip"); err == nil {
		t.Fatal("missing entry accepted")
	}

	if _, err := ParseChecksumFile("zz  FreeIran-v0.9.4-windows-amd64.zip\n", "FreeIran-v0.9.4-windows-amd64.zip"); err == nil {
		t.Fatal("malformed digest accepted")
	}
}

func TestAssetNameMatchesReleasePublishing(t *testing.T) {
	// Contract with release.yml: the updater must name assets exactly
	// as the release pipeline publishes them.
	if AssetName("0.9.4", "windows") != "FreeIran-v0.9.4-windows-amd64.zip" {
		t.Fatalf("AssetName drifted: %s", AssetName("0.9.4", "windows"))
	}

	if InstallerAssetName("0.9.4") != "FreeIran-Setup-v0.9.4-windows-amd64.exe" {
		t.Fatalf("InstallerAssetName drifted: %s", InstallerAssetName("0.9.4"))
	}
}

func TestCheckHonoursContextCancellation(t *testing.T) {
	server := feedServer(t, feedJSON, http.StatusOK)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := httpx.NewClient(httpx.Policy{RequestTimeout: 5 * time.Second})
	defer client.Close()

	if _, err := Check(ctx, client, server.URL, "0.9.4", "windows"); err == nil {
		t.Fatal("cancelled context accepted")
	}
}
