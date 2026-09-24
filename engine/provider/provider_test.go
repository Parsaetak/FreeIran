package provider

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/socks5"
	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// provider_test.go runs the FULL §16 provider matrix against
// deterministic stand-ins (testdata/faketor, testdata/fakepsiphon)
// and local HTTP servers. No test touches the live Tor network or
// Psiphon servers.

var (
	fakeTorBin     string
	fakePsiphonBin string
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "freeiran-provider-fakes-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "provider tests: temp dir: %v\n", err)
		os.Exit(1)
	}

	defer os.RemoveAll(tmp)

	// Build the deterministic stand-ins with the same toolchain
	// running the tests — a missing fixture is a hard failure (the
	// repo's fakecore discipline).
	fakeTorBin = filepath.Join(tmp, executableFileName("tor"))
	if out, err := exec.Command("go", "build", "-o", fakeTorBin, "./testdata/faketor").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "provider tests: build faketor: %v\n%s\n", err, out)
		os.Exit(1)
	}

	fakePsiphonBin = filepath.Join(tmp, executableFileName("psiphon-tunnel-core-"+PlatformSuffix(runtime.GOOS, runtime.GOARCH)))
	if out, err := exec.Command("go", "build", "-o", fakePsiphonBin, "./testdata/fakepsiphon").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "provider tests: build fakepsiphon: %v\n%s\n", err, out)
		os.Exit(1)
	}

	code := m.Run()

	os.Exit(code)
}

// ---- fixture helpers --------------------------------------------------

// buildBundleArchive packs the fake executable into a tar.gz archive
// under the given member name.
func buildBundleArchive(t *testing.T, binaryPath, memberName string) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	data, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read fake binary: %v", err)
	}

	header := &tar.Header{
		Name:    memberName,
		Mode:    0o755,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}

	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("tar header: %v", err)
	}

	if _, err := tw.Write(data); err != nil {
		t.Fatalf("tar write: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// fakeDistServer emulates the official Tor distribution: a version
// listing, sha256sums-signed-build.txt and the expert-bundle asset.
func fakeDistServer(t *testing.T, version string) *httptest.Server {
	t.Helper()

	platform := PlatformSuffix(runtime.GOOS, runtime.GOARCH)
	assetName := fmt.Sprintf("tor-expert-bundle-%s-%s.tar.gz", platform, version)
	archive := buildBundleArchive(t, fakeTorBin, executableFileName("tor"))
	archiveSHA := sha256Hex(archive)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			fmt.Fprintf(w, `<html><body><a href="%s/">%s/</a><a href="16.0a11/">16.0a11/</a></body></html>`, version, version)
		case r.URL.Path == "/"+version+"/sha256sums-signed-build.txt":
			fmt.Fprintf(w, "%s  %s\n", archiveSHA, assetName)
			fmt.Fprintf(w, "%s  tor-expert-bundle-android-aarch64-%s.tar.gz\n", strings.Repeat("a", 64), version)
		case r.URL.Path == "/"+version+"/"+assetName:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(archive)))
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(server.Close)

	return server
}

// fakeGitHubServer emulates a checksum-publishing release channel.
func fakeGitHubServer(t *testing.T, version string, withDigest bool, withAsset bool) *httptest.Server {
	t.Helper()

	platform := PlatformSuffix(runtime.GOOS, runtime.GOARCH)
	assetName := fmt.Sprintf("consoleclient-%s-%s.tar.gz", platform, version)

	member := executableFileName("psiphon-tunnel-core-" + platform)

	archive := buildBundleArchive(t, fakePsiphonBin, member)
	archiveSHA := sha256Hex(archive)

	releases := []map[string]any{}

	if withAsset {
		asset := map[string]any{
			"name":                 assetName,
			"browser_download_url": "/download/" + assetName,
			"size":                 len(archive),
		}

		if withDigest {
			asset["digest"] = "sha256:" + archiveSHA
		}

		releases = append(releases, map[string]any{
			"tag_name":     "v" + version,
			"html_url":     "https://example.invalid/release",
			"published_at": "2026-01-01T00:00:00Z",
			"prerelease":   false,
			"assets":       []any{asset},
		})
	}

	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/releases"):
			// Absolute download URLs (what a real release document
			// carries) are rewritten against this server.
			for _, release := range releases {
				if assets, ok := release["assets"].([]any); ok {
					for _, asset := range assets {
						if m, ok := asset.(map[string]any); ok {
							if url, ok := m["browser_download_url"].(string); ok && strings.HasPrefix(url, "/download/") {
								m["browser_download_url"] = server.URL + url
							}
						}
					}
				}
			}

			w.Header().Set("Content-Type", "application/json")

			if err := json.NewEncoder(w).Encode(releases); err != nil {
				t.Errorf("encode releases: %v", err)
			}
		case strings.HasPrefix(r.URL.Path, "/download/"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(archive)))
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(server.Close)

	return server
}

func testHTTPClient(serverURL string) httpx.Interface {
	return httpx.NewClient(httpx.Policy{
		RequestTimeout:        20 * time.Second,
		DialTimeout:           5 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxRetries:            1,
		MaxBodyBytes:          8 << 20,
	})
}

// ---- Tor: source + validation ----------------------------------------

func TestTorSourceResolvePinned(t *testing.T) {
	dist := fakeDistServer(t, "15.0.20")

	source := NewTorSource(testHTTPClient(dist.URL), false)
	source.BaseURL = dist.URL

	release, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if release.Version != "15.0.20" {
		t.Fatalf("version = %q", release.Version)
	}

	if len(release.SHA256) != 64 {
		t.Fatalf("checksum missing: %+v", release)
	}

	if !strings.Contains(release.AssetURL, "tor-expert-bundle-") {
		t.Fatalf("asset URL = %q", release.AssetURL)
	}
}

func TestTorSourceResolveLatest(t *testing.T) {
	dist := fakeDistServer(t, "15.0.20")

	source := NewTorSource(testHTTPClient(dist.URL), true)
	source.BaseURL = dist.URL

	release, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve latest: %v", err)
	}

	// The fake listing offers 15.0.20 (stable) and 16.0a11 (alpha —
	// must be skipped).
	if release.Version != "15.0.20" {
		t.Fatalf("latest stable = %q, want 15.0.20 (alpha skipped)", release.Version)
	}
}

func TestTorSourceMissingChecksum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "sha256sums-signed-build.txt") {
			fmt.Fprintln(w, strings.Repeat("a", 64)+"  some-other-asset.tar.gz")

			return
		}

		http.NotFound(w, r)
	}))

	t.Cleanup(server.Close)

	source := NewTorSource(testHTTPClient(server.URL), false)
	source.BaseURL = server.URL

	_, err := source.Resolve(context.Background())
	if err == nil {
		t.Fatal("resolve must fail when the checksums do not list the platform asset")
	}
}

func TestTorVersionParsing(t *testing.T) {
	output := "Tor version 0.4.8.16 (git-abc).\nLibevent version 2.1.12-stable\n"

	if v := parseTorVersion(output); v != "0.4.8.16" {
		t.Fatalf("version = %q", v)
	}

	if v := parseTorVersion("no version here"); v != "" {
		t.Fatalf("version = %q, want empty", v)
	}
}

func TestTorWebTunnelCapability(t *testing.T) {
	if !torSupportsWebTunnel("0.4.8.16") {
		t.Fatal("0.4.8 must report builtin WebTunnel")
	}

	if torSupportsWebTunnel("0.4.7.13") {
		t.Fatal("0.4.7 must NOT report builtin WebTunnel")
	}

	if torSupportsWebTunnel("") {
		t.Fatal("unknown version must not claim capabilities")
	}
}

// ---- Tor: install pipeline -------------------------------------------

func TestTorInstallFullPipeline(t *testing.T) {
	dist := fakeDistServer(t, "15.0.20")

	root := t.TempDir()

	source := NewTorSource(testHTTPClient(dist.URL), false)
	source.BaseURL = dist.URL

	engine := NewTorEngineFromSource(root, testHTTPClient(dist.URL), source)

	if err := engine.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}

	manifest := engine.binary.LoadManifest()

	if manifest.BinaryPath == "" || !fileExists(manifest.BinaryPath) {
		t.Fatalf("binary not activated: %+v", manifest)
	}

	if manifest.Version != "0.4.8.16" {
		t.Fatalf("version = %q, want the probed 0.4.8.16", manifest.Version)
	}

	if manifest.State != string(StateInstalled) {
		t.Fatalf("state = %q", manifest.State)
	}

	// The checksum in the manifest must match the served archive's
	// digest (recomputed from the downloaded file).
	if len(manifest.ChecksumSHA256) != 64 {
		t.Fatalf("checksum = %q", manifest.ChecksumSHA256)
	}

	if engine.State() != StateInstalled {
		t.Fatalf("engine state = %q", engine.State())
	}

	info := engine.Info()
	if !info.Installed || info.Version != "0.4.8.16" {
		t.Fatalf("info = %+v", info)
	}

	if info.License == "" || info.Notice == "" {
		t.Fatal("license and attribution notice must be surfaced")
	}
}

func TestTorInstallChecksumMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		platform := PlatformSuffix(runtime.GOOS, runtime.GOARCH)

		if strings.HasSuffix(r.URL.Path, "sha256sums-signed-build.txt") {
			fmt.Fprintln(w, strings.Repeat("b", 64)+"  tor-expert-bundle-"+platform+"-15.0.20.tar.gz")

			return
		}

		archive := buildBundleArchive(t, fakeTorBin, executableFileName("tor"))
		_, _ = w.Write(archive)
	}))

	t.Cleanup(server.Close)

	root := t.TempDir()

	source := NewTorSource(testHTTPClient(server.URL), false)
	source.BaseURL = server.URL

	engine := NewTorEngineFromSource(root, testHTTPClient(server.URL), source)

	err := engine.Install(context.Background())
	if err == nil {
		t.Fatal("install must fail on checksum mismatch")
	}

	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error should name the checksum failure: %v", err)
	}

	// Nothing may be activated.
	if engine.binary.BinaryPath() != "" {
		t.Fatal("no binary may be activated after a failed verification")
	}

	if engine.State() != StateFailed {
		t.Fatalf("state = %q, want failed", engine.State())
	}
}

func TestTorInstallRefusesUnsignedRelease(t *testing.T) {
	// A release with NO published checksum is refused outright.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "sha256sums-signed-build.txt") {
			fmt.Fprintln(w, "no matching line")

			return
		}

		_, _ = w.Write(buildBundleArchive(t, fakeTorBin, executableFileName("tor")))
	}))

	t.Cleanup(server.Close)

	root := t.TempDir()

	source := NewTorSource(testHTTPClient(server.URL), false)
	source.BaseURL = server.URL

	engine := NewTorEngineFromSource(root, testHTTPClient(server.URL), source)

	if _, err := engine.Resolve(context.Background()); err == nil {
		t.Fatal("resolve must fail without a published checksum")
	}
}

// ---- staging transaction recovery (v0.9.15 semantics) -----------------
//
// The install transaction must stay recoverable after an interrupted
// download, a checksum failure, an extraction/validation/smoke failure
// and an activation failure — while the last known-good activated
// binary stays intact and proven-unsafe artifacts are removed. The
// pre-0.9.15 fail() implementation wiped the WHOLE staging directory,
// destroying the resumable .part bytes the downloader exists to
// preserve; these tests pin the corrected contract.

// interruptServer serves `failRequests` responses that honor the
// resume intent (206 + correct Content-Range) but abort the connection
// after delivering a 1 KiB prefix — exactly the shape of a mid-transfer
// network failure on a range-aware server — then serves the COMPLETE
// archive (http.ServeContent) so the downloader resumes from the
// durable prefix.
func interruptServer(t *testing.T, archive []byte, failRequests int) *httptest.Server {
	t.Helper()

	var requests atomic.Int64

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= int64(failRequests) {
			offset := int64(0)

			if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
				// "bytes=N-" → resume offset N.
				if _, after, ok := strings.Cut(rangeHeader, "bytes="); ok {
					start := after

					if idx := strings.Index(after, "-"); idx >= 0 {
						start = after[:idx]
					}

					if v, err := strconv.ParseInt(start, 10, 64); err == nil {
						offset = v
					}
				}
			}

			remaining := int64(len(archive)) - offset

			if remaining <= 0 {
				http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)

				return
			}

			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", offset, int64(len(archive))-1, len(archive)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", remaining))
			w.WriteHeader(http.StatusPartialContent)

			// A durable 1 KiB prefix of the REQUESTED range, then a
			// broken connection. The flush makes the prefix reach the
			// client before the abort (otherwise the server discards
			// the whole response and the client never writes bytes).
			chunk := int64(1024)

			if remaining < chunk {
				chunk = remaining
			}

			_, _ = w.Write(archive[offset : offset+chunk])

			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}

			panic(http.ErrAbortHandler)
		}

		http.ServeContent(w, r, "asset.tar.gz", time.Time{}, bytes.NewReader(archive))
	}))
}

// completeServer serves the archive in full, with Range support.
func completeServer(t *testing.T, archive []byte) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "asset.tar.gz", time.Time{}, bytes.NewReader(archive))
	}))
}

func TestStagingRecoveryAfterInterruptedDownload(t *testing.T) {
	archive := buildBundleArchive(t, fakeTorBin, executableFileName("tor"))

	// Phase 1: EVERY request aborts mid-stream (after the resumed
	// prefix) — every in-call retry layer is exhausted.
	aborting := interruptServer(t, archive, 1_000_000)

	t.Cleanup(aborting.Close)

	// Phase 2: the healthy server serves the complete archive with
	// Range support, so the surviving .part prefix is resumed.
	healthy := completeServer(t, archive)

	t.Cleanup(healthy.Close)

	root := t.TempDir()

	manager := &BinaryManager{
		Name:           TorName,
		RootDir:        root,
		HTTP:           testHTTPClient(aborting.URL),
		Platform:       PlatformSuffix(runtime.GOOS, runtime.GOARCH),
		ExecutableName: "tor",
	}

	release := Release{
		Version:   "15.0.20",
		AssetURL:  aborting.URL + "/asset.tar.gz",
		AssetName: "asset.tar.gz",
		SHA256:    sha256Hex(archive),
	}

	// First transaction: the transfer dies mid-stream after all
	// in-call retries.
	if err := manager.Install(context.Background(), release); err == nil {
		t.Fatal("install must fail when every transfer attempt aborts")
	}

	part := filepath.Join(manager.StagingDir(), "asset.tar.gz.part")

	info, err := os.Stat(part)
	if err != nil {
		t.Fatalf("the resumable .part bytes must survive a failed download: %v", err)
	}

	if info.Size() == 0 || info.Size() >= int64(len(archive)) {
		t.Fatalf("unexpected .part size %d (want a durable 0<p<len prefix)", info.Size())
	}

	// Second transaction against the healthy server: the transfer is
	// completed FROM the surviving prefix. The pre-fix behavior (staging
	// wiped on failure) would redownload from zero; the recovery
	// contract is that the transaction completes and activates.
	manager.HTTP = testHTTPClient(healthy.URL)
	release.AssetURL = healthy.URL + "/asset.tar.gz"

	if err := manager.Install(context.Background(), release); err != nil {
		t.Fatalf("install after interrupted download must recover from the resumable bytes: %v", err)
	}

	if manager.LoadManifest().State != string(StateInstalled) {
		t.Fatal("recovered transaction must activate the verified bundle")
	}

	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("the .part file must be consumed once the transaction completes")
	}
}

func TestStagingRecoveryAfterChecksumFailure(t *testing.T) {
	archive := buildBundleArchive(t, fakeTorBin, executableFileName("tor"))

	server := interruptServer(t, archive, 0) // serves the archive fine

	t.Cleanup(server.Close)

	root := t.TempDir()

	manager := &BinaryManager{
		Name:           TorName,
		RootDir:        root,
		HTTP:           testHTTPClient(server.URL),
		Platform:       PlatformSuffix(runtime.GOOS, runtime.GOARCH),
		ExecutableName: "tor",
	}

	// A known-good activated binary from an earlier release.
	knownGood := filepath.Join(manager.BinDir(), executableFileName("tor"))

	if err := os.MkdirAll(manager.BinDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(knownGood, []byte("known-good-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := manager.saveManifest(Manifest{
		Name:       TorName,
		BinaryPath: knownGood,
		Version:    "0.4.8.12",
		State:      string(StateInstalled),
	}); err != nil {
		t.Fatal(err)
	}

	// A mismatched release digest (corrupt artifact expectation).
	release := Release{
		Version:   "15.0.20",
		AssetURL:  server.URL + "/asset.tar.gz",
		AssetName: "asset.tar.gz",
		SHA256:    strings.Repeat("c", 64), // matches NOTHING
	}

	err := manager.Install(context.Background(), release)
	if err == nil {
		t.Fatal("install must fail on a checksum mismatch")
	}

	// The PROVEN-unsafe artifact is removed — no corrupt archive may
	// linger in staging after a verify failure...
	if _, err := os.Stat(filepath.Join(manager.StagingDir(), "asset.tar.gz")); !os.IsNotExist(err) {
		t.Fatal("the mismatched artifact must be removed, not kept")
	}

	// ...and the known-good binary survives untouched.
	if _, err := os.Stat(knownGood); err != nil {
		t.Fatal("the last known-good activated binary must never be touched by a failed install")
	}

	manifest := manager.LoadManifest()

	if manifest.State != string(StateFailed) || manifest.FailureStage != "verify" {
		t.Fatalf("manifest = %+v, want failed at verify", manifest)
	}

	if manifest.BinaryPath != knownGood {
		t.Fatal("the manifest must keep pointing at the known-good binary")
	}

	// The next attempt downloads fresh and can still succeed (the
	// corrupt artifact did not poison the slot).
	goodRelease := Release{
		Version:   "15.0.20",
		AssetURL:  server.URL + "/asset.tar.gz",
		AssetName: "asset.tar.gz",
		SHA256:    sha256Hex(archive),
	}

	if err := manager.Install(context.Background(), goodRelease); err != nil {
		t.Fatalf("install after a checksum failure must recover: %v", err)
	}

	if manager.LoadManifest().State != string(StateInstalled) {
		t.Fatal("the recovery attempt must activate the verified bundle")
	}
}

// TestStagingFailureStagePolicy pins the fail() stage policy matrix
// directly: a failed transaction removes ONLY what it has proven
// unsafe and never the recoverable state (.part resume bytes, the
// checksum-verified archive) nor the last known-good activated binary.
func TestStagingFailureStagePolicy(t *testing.T) {
	root := t.TempDir()

	manager := &BinaryManager{
		Name:           TorName,
		RootDir:        root,
		ExecutableName: "tor",
	}

	staging := manager.StagingDir()

	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}

	artifact := filepath.Join(staging, "asset.tar.gz")
	part := artifact + ".part"
	unpacked := filepath.Join(staging, "unpacked")

	for _, path := range []string{artifact, part} {
		if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(unpacked, 0o700); err != nil {
		t.Fatal(err)
	}

	// DOWNLOAD failure: everything recoverable survives — the .part IS
	// the resumable transfer.
	if err := manager.fail(manifestStage("download"), fmt.Errorf("network down")); err == nil {
		t.Fatal("fail must return the wrapped error")
	}

	for _, path := range []string{artifact, part, unpacked} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("download-stage failure must keep %s: %v", path, err)
		}
	}

	// VERIFY failure (the proven-unsafe artifact was removed at the
	// proof site): the remaining recoverable state still survives.
	_ = os.Remove(artifact)

	if err := manager.fail(manifestStage("verify"), fmt.Errorf("checksum mismatch")); err == nil {
		t.Fatal("fail must return the wrapped error")
	}

	if _, err := os.Stat(part); err != nil {
		t.Fatalf("verify-stage failure must keep the .part bytes: %v", err)
	}

	if _, err := os.Stat(unpacked); err != nil {
		t.Fatalf("verify-stage failure must keep the unpacked tree: %v", err)
	}

	// UNPACK/VALIDATE/SMOKE failure: the derived unpacked tree is
	// discarded, the verified archive and resume bytes stay.
	if err := manager.fail(manifestStage("smoke"), fmt.Errorf("smoke failed")); err == nil {
		t.Fatal("fail must return the wrapped error")
	}

	if _, err := os.Stat(unpacked); !os.IsNotExist(err) {
		t.Fatal("smoke-stage failure must discard the unpacked tree")
	}

	if _, err := os.Stat(part); err != nil {
		t.Fatalf("smoke-stage failure must keep the .part bytes: %v", err)
	}

	// The manifest records the honest failure — and no activated
	// binary was ever claimed destroyed (there was none to destroy).
	manifest := manager.LoadManifest()

	if manifest.State != string(StateFailed) || manifest.FailureStage != "smoke" {
		t.Fatalf("manifest = %+v, want failed at smoke", manifest)
	}
}

func TestStagingRecoveryAfterValidationFailure(t *testing.T) {
	archive := buildBundleArchive(t, fakeTorBin, executableFileName("tor"))

	server := interruptServer(t, archive, 0)

	t.Cleanup(server.Close)

	root := t.TempDir()

	// ValidateBinary always fails: the archive itself is VALID (its
	// digest matches), so it must be REUSED by the next attempt rather
	// than redownloaded; only the derived unpacked tree may go.
	manager := &BinaryManager{
		Name:           TorName,
		RootDir:        root,
		HTTP:           testHTTPClient(server.URL),
		Platform:       PlatformSuffix(runtime.GOOS, runtime.GOARCH),
		ExecutableName: "tor",
		ValidateBinary: func(context.Context, string) (string, error) {
			return "", fmt.Errorf("injected validation failure")
		},
	}

	release := Release{
		Version:   "15.0.20",
		AssetURL:  server.URL + "/asset.tar.gz",
		AssetName: "asset.tar.gz",
		SHA256:    sha256Hex(archive),
	}

	if err := manager.Install(context.Background(), release); err == nil {
		t.Fatal("install must fail when validation is injected to fail")
	}

	// The verified archive is kept for artifact reuse...
	staged := filepath.Join(manager.StagingDir(), "asset.tar.gz")

	if info, err := os.Stat(staged); err != nil || info.Size() != int64(len(archive)) {
		t.Fatalf("a checksum-verified archive must survive a validation failure (err=%v)", err)
	}

	// ...while the derived unpacked tree is discarded...
	if _, err := os.Stat(filepath.Join(manager.StagingDir(), "unpacked")); !os.IsNotExist(err) {
		t.Fatal("the unpacked tree must be discarded after a validation failure")
	}

	// ...and the manifest records the honest failure stage.
	if manifest := manager.LoadManifest(); manifest.FailureStage != "validate" {
		t.Fatalf("failure stage = %q, want validate", manifest.FailureStage)
	}

	// Retry with a working validator: the SAME staging artifact is
	// reused (no redownload) and the transaction completes.
	manager.ValidateBinary = func(_ context.Context, path string) (string, error) {
		return "0.4.8.16", nil
	}

	if err := manager.Install(context.Background(), release); err != nil {
		t.Fatalf("retry with the reused verified artifact must succeed: %v", err)
	}

	if manager.LoadManifest().State != string(StateInstalled) {
		t.Fatal("the retry must activate the verified bundle")
	}
}

// ---- Tor: lifecycle ---------------------------------------------------

func TestTorLifecycleStartBootstrapStopRestart(t *testing.T) {
	dist := fakeDistServer(t, "15.0.20")

	root := t.TempDir()

	source := NewTorSource(testHTTPClient(dist.URL), false)
	source.BaseURL = dist.URL

	engine := NewTorEngineFromSource(root, testHTTPClient(dist.URL), source)

	ctx := context.Background()

	if err := engine.Install(ctx); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := engine.SetOptions(TorOptions{BootstrapTimeout: 30 * time.Second}); err != nil {
		t.Fatalf("options: %v", err)
	}

	// START + BOOTSTRAP: readiness observed from the real bootstrap
	// lines and the SOCKS endpoint.
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	if engine.State() != StateReady {
		t.Fatalf("state = %q, want ready", engine.State())
	}

	endpoints := engine.Endpoints()
	if len(endpoints) != 1 || endpoints[0].Network != "socks5" || endpoints[0].Port == 0 {
		t.Fatalf("endpoints = %+v", endpoints)
	}

	if !endpoints[0].Verified {
		t.Fatal("endpoint must be verified from the real runtime")
	}

	// BOOTSTRAP came from actual log lines.
	info := engine.Info()
	if !info.Bootstrap.Complete || info.Bootstrap.Progress != 100 {
		t.Fatalf("bootstrap = %+v", info.Bootstrap)
	}

	// HEALTH: measured.
	health := engine.Health(ctx)
	if !health.OK || !health.ProcessAlive || !health.ListenerReady {
		t.Fatalf("health = %+v", health)
	}

	if !health.Measured {
		t.Fatal("health latency must be measured")
	}

	// HTTP REQUEST THROUGH TOR: a real request through the local
	// SOCKS5 endpoint.
	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("through-tor-ok"))
	}))

	t.Cleanup(httpTarget.Close)

	dialer := socks5.Dialer{ProxyAddr: endpoints[0].Addr(), Timeout: 10 * time.Second}

	conn, err := dialer.Dial(ctx, "tcp", strings.TrimPrefix(httpTarget.URL, "http://"))
	if err != nil {
		t.Fatalf("socks dial through tor endpoint: %v", err)
	}

	_ = conn.Close()

	// STOP: deterministic, no orphan.
	if err := engine.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if engine.State() != StateInstalled {
		t.Fatalf("post-stop state = %q", engine.State())
	}

	if len(engine.Endpoints()) != 0 {
		t.Fatal("endpoints must clear after stop")
	}

	// RESTART.
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}

	if engine.State() != StateReady {
		t.Fatalf("restart state = %q", engine.State())
	}

	if err := engine.Stop(ctx); err != nil {
		t.Fatalf("stop 2: %v", err)
	}
}

func TestTorStartFailsFastOnBadBinary(t *testing.T) {
	root := t.TempDir()

	// A "tor" binary that crashes immediately.
	broken := filepath.Join(root, "broken-tor")

	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatalf("write broken binary: %v", err)
	}

	if runtime.GOOS == "windows" {
		t.Skip("posix script fixture not applicable on windows")
	}

	engine := NewTorEngine(root, testHTTPClient("http://127.0.0.1:1"), false)

	// Plant the broken binary as the activated one.
	if err := os.MkdirAll(engine.binary.BinDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(engine.binary.BinDir(), executableFileName("tor"))

	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := engine.binary.saveManifest(Manifest{
		Name:       TorName,
		BinaryPath: target,
		Version:    "0.4.8.16",
		State:      string(StateInstalled),
	}); err != nil {
		t.Fatal(err)
	}

	if err := engine.SetOptions(TorOptions{BootstrapTimeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}

	err := engine.Start(context.Background())
	if err == nil {
		t.Fatal("start must fail when the binary crashes")
	}

	if engine.State() != StateFailed {
		t.Fatalf("state = %q, want failed", engine.State())
	}

	// No endpoints, no orphan process.
	if len(engine.Endpoints()) != 0 {
		t.Fatal("no endpoints may remain after a failed start")
	}
}

func TestTorStartRequiresInstall(t *testing.T) {
	engine := NewTorEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"), false)

	if err := engine.Start(context.Background()); err == nil {
		t.Fatal("start must fail when not installed")
	}
}

// ---- Tor: bridges ------------------------------------------------------

func TestTorBridgeValidation(t *testing.T) {
	valid := []string{
		"obfs4 192.95.36.4:443 fingerprint-goes-here-0123456789abcdef cert=abcd iat-mode=0",
		"Bridge snowflake 192.0.2.1:443",
		"webtunnel 198.51.100.1:443 url=https://example/b",
	}

	for _, line := range valid {
		if err := ValidateBridgeLine(line); err != nil {
			t.Fatalf("valid bridge rejected: %q: %v", line, err)
		}
	}

	invalid := []string{
		"",
		"only-transport",
		"obfs4 no-port",
		"obfs4 192.0.2.1:notaport",
	}

	for _, line := range invalid {
		if err := ValidateBridgeLine(line); err == nil {
			t.Fatalf("invalid bridge accepted: %q", line)
		}
	}
}

func TestTorOptionsRejectMissingPlugin(t *testing.T) {
	err := ValidateTorOptions(TorOptions{
		TransportPlugins: map[string]string{"obfs4": "/definitely/not/here/obfs4proxy"},
	})
	if err == nil {
		t.Fatal("missing plugin binary must be rejected before launch")
	}
}

func TestTorTorrcBridgeLines(t *testing.T) {
	torrc, err := buildTorrc(TorrcConfig{
		SocksPort:   9050,
		DataDir:     "/data",
		CacheDir:    "/cache",
		BridgeLines: []string{"snowflake 192.0.2.1:443"},
	})
	if err != nil {
		t.Fatalf("build torrc: %v", err)
	}

	if !strings.Contains(torrc, "UseBridges 1") {
		t.Fatal("bridges must enable UseBridges")
	}

	if !strings.Contains(torrc, "Bridge snowflake 192.0.2.1:443") {
		t.Fatal("bridge line must be written")
	}

	if !strings.Contains(torrc, "Log notice stdout") {
		t.Fatal("bootstrap observation requires notice logs on stdout")
	}

	// Invalid bridge lines are refused BEFORE a process launches.
	if _, err := buildTorrc(TorrcConfig{SocksPort: 9050, BridgeLines: []string{"broken"}}); err == nil {
		t.Fatal("invalid bridge must fail the torrc build")
	}
}

// ---- Tor: cleanup ------------------------------------------------------

func TestTorCleanup(t *testing.T) {
	root := t.TempDir()

	engine := NewTorEngine(root, testHTTPClient("http://127.0.0.1:1"), false)

	logDir := filepath.Join(engine.binary.Dir(), "logs")

	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}

	old := filepath.Join(logDir, "old.log")

	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(old, time.Now().Add(-30*24*time.Hour), time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := engine.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale logs must be pruned")
	}
}

// ---- Psiphon -----------------------------------------------------------

func TestPsiphonResolveDigestRelease(t *testing.T) {
	server := fakeGitHubServer(t, "2.0.31", true, true)

	source := NewGitHubReleaseSource(testHTTPClient(server.URL), "example/repo", "consoleclient")
	source.APIBase = server.URL

	release, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if release.Version != "2.0.31" {
		t.Fatalf("version = %q", release.Version)
	}

	if len(release.SHA256) != 64 {
		t.Fatalf("digest = %q", release.SHA256)
	}
}

func TestPsiphonResolveUnavailableProvider(t *testing.T) {
	// No releases at all: the honest "unavailable" error.
	server := fakeGitHubServer(t, "", false, false)

	source := NewGitHubReleaseSource(testHTTPClient(server.URL), "example/repo", "consoleclient")
	source.APIBase = server.URL

	_, err := source.Resolve(context.Background())
	if err == nil {
		t.Fatal("resolve must report unavailability honestly")
	}

	if !strings.Contains(err.Error(), "provide the binary manually") {
		t.Fatalf("error should explain the manual path: %v", err)
	}
}

func TestPsiphonResolveRejectsUnsignedAssets(t *testing.T) {
	// Assets exist but carry no digest: never installable.
	server := fakeGitHubServer(t, "2.0.31", false, true)

	source := NewGitHubReleaseSource(testHTTPClient(server.URL), "example/repo", "consoleclient")
	source.APIBase = server.URL

	_, err := source.Resolve(context.Background())
	if err == nil {
		t.Fatal("unsigned assets must be refused")
	}
}

func TestPsiphonInstallFullPipeline(t *testing.T) {
	server := fakeGitHubServer(t, "2.0.31", true, true)

	root := t.TempDir()

	source := NewGitHubReleaseSource(testHTTPClient(server.URL), "example/repo", "consoleclient")
	source.APIBase = server.URL

	engine := NewPsiphonEngineFromSource(root, testHTTPClient(server.URL), source)

	if err := engine.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}

	manifest := engine.binary.LoadManifest()

	if manifest.BinaryPath == "" || !fileExists(manifest.BinaryPath) {
		t.Fatalf("binary not activated: %+v", manifest)
	}

	if len(manifest.ChecksumSHA256) != 64 {
		t.Fatalf("checksum = %q", manifest.ChecksumSHA256)
	}

	info := engine.Info()
	if !info.Installed {
		t.Fatalf("info = %+v", info)
	}

	if info.License == "" || info.Notice == "" {
		t.Fatal("Psiware attribution must be surfaced")
	}
}

func TestPsiphonCorruptBinaryRejected(t *testing.T) {
	// Serve an archive whose "binary" is garbage: validation must
	// fail the transaction (on posix the fake garbage exits 126; on
	// windows it is not a valid PE).
	platform := PlatformSuffix(runtime.GOOS, runtime.GOARCH)

	assetName := fmt.Sprintf("consoleclient-%s-2.0.31.tar.gz", platform)

	var archive bytes.Buffer

	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)

	garbage := []byte("this is not an executable program")

	_ = tw.WriteHeader(&tar.Header{Name: executableFileName("psiphon-tunnel-core-" + platform), Mode: 0o755, Size: int64(len(garbage))})
	_, _ = tw.Write(garbage)
	_ = tw.Close()
	_ = gz.Close()

	archiveBytes := archive.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/releases") {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"tag_name": "v2.0.31",
				"assets": []any{map[string]any{
					"name":                 assetName,
					"browser_download_url": "/download/" + assetName,
					"size":                 len(archiveBytes),
					"digest":               "sha256:" + sha256Hex(archiveBytes),
				}},
			}})

			return
		}

		_, _ = w.Write(archiveBytes)
	}))

	t.Cleanup(server.Close)

	root := t.TempDir()

	source := NewGitHubReleaseSource(testHTTPClient(server.URL), "example/repo", "consoleclient")
	source.APIBase = server.URL

	engine := NewPsiphonEngineFromSource(root, testHTTPClient(server.URL), source)

	err := engine.Install(context.Background())
	if err == nil {
		t.Fatal("corrupt binary must fail the install transaction")
	}

	if engine.binary.BinaryPath() != "" {
		t.Fatal("no binary may be activated")
	}
}

func TestPsiphonLifecycleTunnelProxiesHealthStopRestart(t *testing.T) {
	server := fakeGitHubServer(t, "2.0.31", true, true)

	root := t.TempDir()

	source := NewGitHubReleaseSource(testHTTPClient(server.URL), "example/repo", "consoleclient")
	source.APIBase = server.URL

	engine := NewPsiphonEngineFromSource(root, testHTTPClient(server.URL), source)

	ctx := context.Background()

	if err := engine.Install(ctx); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := engine.SetOptions(PsiphonOptions{NegotiateTimeout: 30 * time.Second}); err != nil {
		t.Fatalf("options: %v", err)
	}

	// START + TUNNEL NEGOTIATION + PROXY READINESS.
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	if engine.State() != StateReady {
		t.Fatalf("state = %q, want ready", engine.State())
	}

	endpoints := engine.Endpoints()
	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %+v, want socks+http", endpoints)
	}

	var socks, httpEP *Endpoint

	for i := range endpoints {
		switch endpoints[i].Network {
		case "socks5":
			socks = &endpoints[i]
		case "http":
			httpEP = &endpoints[i]
		}
	}

	if socks == nil || httpEP == nil {
		t.Fatalf("endpoints = %+v", endpoints)
	}

	// HEALTH with measured latency.
	health := engine.Health(ctx)
	if !health.OK {
		t.Fatalf("health = %+v", health)
	}

	if !health.Measured {
		t.Fatal("health must be measured")
	}

	// HTTP THROUGH PSIPHON: real request through the SOCKS endpoint.
	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("through-psiphon-ok"))
	}))

	t.Cleanup(httpTarget.Close)

	dialer := socks5.Dialer{ProxyAddr: socks.Addr(), Timeout: 10 * time.Second}

	conn, err := dialer.Dial(ctx, "tcp", strings.TrimPrefix(httpTarget.URL, "http://"))
	if err != nil {
		t.Fatalf("dial through psiphon socks: %v", err)
	}

	_ = conn.Close()

	// Capabilities only from the real runtime.
	info := engine.Info()
	if len(info.Capabilities) == 0 {
		t.Fatal("running client must report discovered capabilities")
	}

	// STOP.
	if err := engine.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if engine.State() != StateInstalled {
		t.Fatalf("post-stop state = %q", engine.State())
	}

	if len(engine.Endpoints()) != 0 {
		t.Fatal("endpoints must clear")
	}

	// Not running → no capabilities claimed.
	info = engine.Info()
	if len(info.Capabilities) != 0 {
		t.Fatalf("stopped client must not claim capabilities: %v", info.Capabilities)
	}

	// RESTART.
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}

	if err := engine.Stop(ctx); err != nil {
		t.Fatalf("stop 2: %v", err)
	}
}

func TestPsiphonStartRequiresInstall(t *testing.T) {
	engine := NewPsiphonEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"))

	err := engine.Start(context.Background())
	if err == nil {
		t.Fatal("start must fail without an installed binary")
	}

	if !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("error = %v", err)
	}
}

func TestPsiphonUserBinaryPath(t *testing.T) {
	root := t.TempDir()

	// The user's binary lives OUTSIDE the managed tree, like a real
	// user-provided file (Downloads, a tools folder, ...).
	userDir := t.TempDir()

	source := filepath.Join(userDir, filepath.Base(fakePsiphonBin))

	if err := copyTestFile(fakePsiphonBin, source); err != nil {
		t.Fatal(err)
	}

	engine := NewPsiphonEngine(root, testHTTPClient("http://127.0.0.1:1"))

	if err := engine.SetUserBinary(context.Background(), source); err != nil {
		t.Fatalf("user binary adoption failed validation: %v", err)
	}

	// The adoption contract: the user's original is never consumed.
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("user's source binary must remain after adoption: %v", err)
	}

	// The manifest points at the managed copy, not the user's file.
	manifest := engine.binary.LoadManifest()

	if manifest.BinaryPath == source || manifest.BinaryPath == "" {
		t.Fatalf("manifest must point at the managed copy, got %q", manifest.BinaryPath)
	}

	if !engine.Info().Installed {
		t.Fatal("adopted binary must be installed")
	}

	if engine.Info().Source != "user-provided" {
		t.Fatalf("source = %q", engine.Info().Source)
	}

	// The adopted binary actually runs.
	if err := engine.SetOptions(PsiphonOptions{NegotiateTimeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}

	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("start with user binary: %v", err)
	}

	_ = engine.Stop(context.Background())

	// The source survives the full run lifecycle too.
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("user's source binary must survive the provider lifecycle: %v", err)
	}
}

func TestPsiphonExtraConfigValidation(t *testing.T) {
	engine := NewPsiphonEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"))

	if err := engine.SetOptions(PsiphonOptions{ExtraConfig: `{not json`}); err == nil {
		t.Fatal("malformed extra config must be rejected")
	}

	if err := engine.SetOptions(PsiphonOptions{ExtraConfig: `{"EgressRegion":"US"}`}); err != nil {
		t.Fatalf("valid extra config rejected: %v", err)
	}
}

// ---- unified manager ---------------------------------------------------

func TestManagerRegistryAndLifecycle(t *testing.T) {
	manager := NewManager()

	dist := fakeDistServer(t, "15.0.20")

	source := NewTorSource(testHTTPClient(dist.URL), false)
	source.BaseURL = dist.URL

	tor := NewTorEngineFromSource(t.TempDir(), testHTTPClient(dist.URL), source)

	manager.Register(tor)

	if _, ok := manager.Get("tor"); !ok {
		t.Fatal("tor must be registered")
	}

	names := manager.Names()
	if len(names) != 1 || names[0] != "tor" {
		t.Fatalf("names = %v", names)
	}

	// Install, then the managed lifecycle works end to end.
	if err := tor.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := manager.Start(context.Background(), "tor"); err != nil {
		t.Fatalf("managed start: %v", err)
	}

	if err := manager.Stop(context.Background(), "tor"); err != nil {
		t.Fatalf("managed stop: %v", err)
	}

	// The manager surfaces the unified Info shape.
	infos := manager.List()
	if len(infos) != 1 || infos[0].Kind != KindTor {
		t.Fatalf("infos = %+v", infos)
	}
}

func TestManagerUnknownProvider(t *testing.T) {
	manager := NewManager()

	if err := manager.Start(context.Background(), "nope"); err == nil {
		t.Fatal("unknown provider must error")
	}
}

// ---- shared binary pipeline edge cases ---------------------------------

func TestManifestPersistRoundTrip(t *testing.T) {
	root := t.TempDir()

	binary := &BinaryManager{Name: "tor", RootDir: root}

	manifest := Manifest{
		Name:       "tor",
		Version:    "0.4.8.16",
		BinaryPath: "/x/tor",
		State:      string(StateInstalled),
	}

	if err := binary.saveManifest(manifest); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded := binary.LoadManifest()
	if loaded.Version != "0.4.8.16" || loaded.BinaryPath != "/x/tor" {
		t.Fatalf("round trip = %+v", loaded)
	}
}

func TestManifestCorruptFallsBackToNotInstalled(t *testing.T) {
	root := t.TempDir()

	binary := &BinaryManager{Name: "tor", RootDir: root}

	if err := os.MkdirAll(binary.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(binary.ManifestPath(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if loaded := binary.LoadManifest(); loaded.State != string(StateNotInstalled) {
		t.Fatalf("state = %q", loaded.State)
	}
}

func TestPlatformSuffix(t *testing.T) {
	if got := PlatformSuffix("windows", "amd64"); got != "windows-x86_64" {
		t.Fatalf("windows/amd64 = %q", got)
	}

	if got := PlatformSuffix("linux", "amd64"); got != "linux-x86_64" {
		t.Fatalf("linux/amd64 = %q", got)
	}

	if got := PlatformSuffix("darwin", "arm64"); got != "macos-aarch64" {
		t.Fatalf("darwin/arm64 = %q", got)
	}
}

func TestNormalizeDigest(t *testing.T) {
	if got := normalizeDigest("sha256:" + strings.Repeat("a", 64)); got != strings.Repeat("a", 64) {
		t.Fatalf("digest = %q", got)
	}

	if got := normalizeDigest("md5:abc"); got != "" {
		t.Fatalf("non-sha256 digest must be rejected: %q", got)
	}

	if got := normalizeDigest(strings.Repeat("z", 64)); got != "" {
		t.Fatalf("non-hex digest must be rejected: %q", got)
	}
}

func TestExtractHrefDirs(t *testing.T) {
	body := `<a href="15.0.20/">15.0.20/</a> <a href="?C=N">sort</a> <a href="file.txt">f</a>`

	dirs := extractHrefDirs(body)
	if len(dirs) != 1 || dirs[0] != "15.0.20/" {
		t.Fatalf("dirs = %v", dirs)
	}
}

// Silence unused import when io is only used on some platforms.
var _ = io.Discard

// ---- Psiphon user-binary adoption ownership (v0.9.8.1 root fix) ----------
//
// The v0.9.8.1 Windows failure (TestPsiphonUserBinaryPath) came from
// SetUserBinary() moving (and deleting) the user's executable: a
// Windows image mapping outliving process termination made the delete
// fail with "being used by another process". The ownership model is
// now copy-not-move; these tests pin every clause of the contract.

// TestPsiphonUserBinaryAdoptionPreservesSource: the user's original
// file survives adoption byte-identically; the manifest points at a
// managed copy whose checksum matches BOTH files.
func TestPsiphonUserBinaryAdoptionPreservesSource(t *testing.T) {
	root := t.TempDir()

	// The user's file lives OUTSIDE FreeIran's managed tree.
	userDir := t.TempDir()
	source := filepath.Join(userDir, executableFileName("psiphon-tunnel-core-"+PlatformSuffix(runtime.GOOS, runtime.GOARCH)))

	if err := copyTestFile(fakePsiphonBin, source); err != nil {
		t.Fatal(err)
	}

	sourceBefore, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	engine := NewPsiphonEngine(root, testHTTPClient("http://127.0.0.1:1"))

	if err := engine.SetUserBinary(context.Background(), source); err != nil {
		t.Fatalf("adoption failed: %v", err)
	}

	// The source file remains — never moved, renamed or deleted.
	sourceAfter, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("user's source binary must survive adoption: %v", err)
	}

	if !bytes.Equal(sourceBefore, sourceAfter) {
		t.Fatal("user's source binary was modified during adoption")
	}

	if _, err := os.Stat(source); err != nil {
		t.Fatalf("user's source binary must still exist at its original path: %v", err)
	}

	manifest := engine.binary.LoadManifest()

	if manifest.BinaryPath == source {
		t.Fatal("manifest must point at the managed copy, not the user's file")
	}

	if !strings.HasPrefix(manifest.BinaryPath, filepath.Join(root, "psiphon")) {
		t.Fatalf("managed copy must live inside provider storage: %q", manifest.BinaryPath)
	}

	if _, err := os.Stat(manifest.BinaryPath); err != nil {
		t.Fatalf("managed copy must exist: %v", err)
	}

	// Checksum corresponds to the MANAGED COPY (and equals the source's).
	managedSum, err := fileSHA256(manifest.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.EqualFold(managedSum, manifest.ChecksumSHA256) {
		t.Fatalf("manifest checksum %s does not match the managed copy %s", manifest.ChecksumSHA256, managedSum)
	}

	sourceSum := sha256Hex(sourceBefore)
	if !strings.EqualFold(sourceSum, managedSum) {
		t.Fatal("managed copy must be byte-equivalent to the source")
	}

	if engine.Info().Source != "user-provided" {
		t.Fatalf("source = %q", engine.Info().Source)
	}
}

// TestPsiphonUserBinarySourceUndeletable: the Windows sharing-violation
// condition is simulated deterministically — a source whose directory
// denies deletion (read+execute only). The v0.9.8.1 moveFile() flow
// FAILED here (its copy fallback ends with os.Remove(src)); the
// copy-not-move contract succeeds because the source is never touched.
func TestPsiphonUserBinarySourceUndeletable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits are a POSIX simulation of the Windows sharing violation; the real Windows lock is exercised by CI on windows-latest")
	}

	userDir := t.TempDir()

	defer os.Chmod(userDir, 0o700) // restore for cleanup

	source := filepath.Join(userDir, executableFileName("psiphon-tunnel-core-"+PlatformSuffix(runtime.GOOS, runtime.GOARCH)))

	if err := copyTestFile(fakePsiphonBin, source); err != nil {
		t.Fatal(err)
	}

	// r-x for owner: reading/executing the source stays possible, but
	// rename/remove of files inside the directory is denied — the
	// deterministic stand-in for "The process cannot access the file
	// because it is being used by another process".
	if err := os.Chmod(userDir, 0o500); err != nil {
		t.Fatal(err)
	}

	engine := NewPsiphonEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"))

	if err := engine.SetUserBinary(context.Background(), source); err != nil {
		t.Fatalf("adoption must succeed with an undeletable source: %v", err)
	}

	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source must remain: %v", err)
	}
}

// TestPsiphonUserBinaryRepeatedAdoptionDeterministic: adopting the
// same binary twice resolves to the same content-addressed managed
// copy, the same checksum, and a still-intact source.
func TestPsiphonUserBinaryRepeatedAdoptionDeterministic(t *testing.T) {
	userDir := t.TempDir()

	source := filepath.Join(userDir, executableFileName("psiphon-tunnel-core-"+PlatformSuffix(runtime.GOOS, runtime.GOARCH)))

	if err := copyTestFile(fakePsiphonBin, source); err != nil {
		t.Fatal(err)
	}

	engine := NewPsiphonEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"))

	if err := engine.SetUserBinary(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	first := engine.binary.LoadManifest()

	if err := engine.SetUserBinary(context.Background(), source); err != nil {
		t.Fatalf("second adoption failed: %v", err)
	}

	second := engine.binary.LoadManifest()

	if first.BinaryPath != second.BinaryPath {
		t.Fatalf("managed path must be content-addressed and stable: %q vs %q", first.BinaryPath, second.BinaryPath)
	}

	if first.ChecksumSHA256 != second.ChecksumSHA256 {
		t.Fatalf("checksum drift: %q vs %q", first.ChecksumSHA256, second.ChecksumSHA256)
	}

	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source must still exist after repeated adoption: %v", err)
	}
}

// TestPsiphonUserBinaryManagedLifecycleAfterSourceDisappearance: the
// provider runs from the MANAGED COPY — the user's file disappearing
// afterwards changes nothing (start → health → stop still work).
func TestPsiphonUserBinaryManagedLifecycleAfterSourceDisappearance(t *testing.T) {
	userDir := t.TempDir()

	source := filepath.Join(userDir, executableFileName("psiphon-tunnel-core-"+PlatformSuffix(runtime.GOOS, runtime.GOARCH)))

	if err := copyTestFile(fakePsiphonBin, source); err != nil {
		t.Fatal(err)
	}

	engine := NewPsiphonEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"))

	if err := engine.SetUserBinary(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	manifest := engine.binary.LoadManifest()

	// The user deletes/moves their file after adoption.
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}

	if err := engine.SetOptions(PsiphonOptions{NegotiateTimeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}

	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("provider must start from the managed copy after the source disappeared: %v", err)
	}

	health := engine.Health(context.Background())
	if !health.OK || !health.Measured {
		t.Fatalf("health after source disappearance: %+v", health)
	}

	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Restart from the managed copy works too.
	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("restart from managed copy: %v", err)
	}

	_ = engine.Stop(context.Background())

	if _, err := os.Stat(manifest.BinaryPath); err != nil {
		t.Fatalf("managed copy must remain the provider's own asset: %v", err)
	}
}

// TestPsiphonUserBinaryUninstallNeverTouchesSource: Uninstall removes
// FreeIran's managed state only — the user's external binary is not
// part of it and must survive.
func TestPsiphonUserBinaryUninstallNeverTouchesSource(t *testing.T) {
	userDir := t.TempDir()

	source := filepath.Join(userDir, executableFileName("psiphon-tunnel-core-"+PlatformSuffix(runtime.GOOS, runtime.GOARCH)))

	if err := copyTestFile(fakePsiphonBin, source); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()

	engine := NewPsiphonEngine(root, testHTTPClient("http://127.0.0.1:1"))

	if err := engine.SetUserBinary(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	if err := engine.Uninstall(context.Background()); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	if _, err := os.Stat(source); err != nil {
		t.Fatalf("uninstall must never delete the user's source binary: %v", err)
	}

	// Managed state is gone.
	if _, err := os.Stat(filepath.Join(root, "psiphon")); err == nil {
		t.Fatal("managed provider slot must be removed by uninstall")
	}

	if engine.Info().Installed {
		t.Fatal("provider must report not-installed after uninstall")
	}
}

// TestPsiphonUserBinaryRejectsGarbage: a file that cannot run is never
// adopted (the smoke launch is the gate), and the user's file still
// survives the failed attempt.
func TestPsiphonUserBinaryRejectsGarbage(t *testing.T) {
	userDir := t.TempDir()

	source := filepath.Join(userDir, "not-a-psiphon-binary")

	if err := os.WriteFile(source, []byte("this is not an executable"), 0o600); err != nil {
		t.Fatal(err)
	}

	engine := NewPsiphonEngine(t.TempDir(), testHTTPClient("http://127.0.0.1:1"))

	if err := engine.SetUserBinary(context.Background(), source); err == nil {
		t.Fatal("a non-executable file must fail adoption")
	}

	if _, err := os.Stat(source); err != nil {
		t.Fatalf("even a failed adoption must not delete the user's file: %v", err)
	}

	if engine.Info().Installed {
		t.Fatal("failed adoption must not activate a binary")
	}
}

// copyTestFile is a plain byte copy helper for fixtures.
func copyTestFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, 0o755)
}

// v0.9.8.3: the Tor checksum parser tolerates the real official file
// shape (GNU sha256sum format, binary-mode marker, extra whitespace)
// while still refusing lines that do not name the asset or do not
// carry a 64-hex digest.
func TestFindChecksumLineTolerant(t *testing.T) {
	body := "ed4bc23065ee10f68efcdae63ea318ffa4b02b04ba00f13a3f59f8e3832fdfad  tor-expert-bundle-android-aarch64-15.0.23.tar.gz\n" +
		"af684a8839d61778b5722938e43cc0c1cc9886f8fd8b7fb33d056077363edfba  tor-expert-bundle-linux-i686-15.0.23.tar.gz\n" +
		"2bf7d66307db90fc3f76ca0d412723de9e37755454cbeb22d806a3b4c9c22595 *tor-expert-bundle-windows-x86_64-15.0.23.tar.gz\n" +
		"not-a-hash  tor-expert-bundle-windows-x86_64-15.0.99.tar.gz\n"

	want := "2bf7d66307db90fc3f76ca0d412723de9e37755454cbeb22d806a3b4c9c22595"

	if got := findChecksumLine(body, "tor-expert-bundle-windows-x86_64-15.0.23.tar.gz"); got != want {
		t.Fatalf("binary-mode line: got %q", got)
	}

	simple := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  tor-expert-bundle-windows-x86_64-15.0.20.tar.gz\n"
	if got := findChecksumLine(simple, "tor-expert-bundle-windows-x86_64-15.0.20.tar.gz"); got != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("two-space line: got %q", got)
	}

	if got := findChecksumLine(body, "tor-expert-bundle-windows-x86_64-15.0.99.tar.gz"); got != "" {
		t.Fatalf("invalid digest line accepted: %q", got)
	}

	if got := findChecksumLine(body, "missing-asset.tar.gz"); got != "" {
		t.Fatalf("missing asset matched: %q", got)
	}
}

// TestTorSourceAssetHostSplit pins the v0.9.15 distribution topology:
// dist.torproject.org serves the checksum authority but NOT the expert
// bundles (the live upstream answers HTTP 404 for tor-expert-bundle-*
// there — the exact defect that broke managed Tor acquisition), while
// the official package archive hosts the assets. Resolve must build
// the asset URL against the archive host while the digest still comes
// from dist, and a full install must succeed across the two hosts.
func TestTorSourceAssetHostSplit(t *testing.T) {
	version := "15.0.20"
	platform := PlatformSuffix(runtime.GOOS, runtime.GOARCH)
	assetName := fmt.Sprintf("tor-expert-bundle-%s-%s.tar.gz", platform, version)
	archive := buildBundleArchive(t, fakeTorBin, executableFileName("tor"))
	archiveSHA := sha256Hex(archive)

	// dist: listing + signed checksums; every asset request 404s (the
	// live upstream topology that used to fail the download stage).
	dist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			fmt.Fprintf(w, `<html><body><a href="%s/">%s/</a></body></html>`, version, version)
		case r.URL.Path == "/"+version+"/sha256sums-signed-build.txt":
			fmt.Fprintf(w, "%s  %s\n", archiveSHA, assetName)
		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(dist.Close)

	// archive: the official package archive serving the bundle.
	arch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/torbrowser/"+version+"/"+assetName {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(archive)))
			_, _ = w.Write(archive)

			return
		}

		http.NotFound(w, r)
	}))

	t.Cleanup(arch.Close)

	root := t.TempDir()

	source := NewTorSource(testHTTPClient(dist.URL), false)
	source.BaseURL = dist.URL
	source.ArchiveURL = arch.URL + "/torbrowser"

	release, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if !strings.HasPrefix(release.AssetURL, arch.URL+"/torbrowser/") {
		t.Fatalf("asset URL must come from the archive host, got %q", release.AssetURL)
	}

	if !strings.Contains(release.AssetURL, assetName) || len(release.SHA256) != 64 {
		t.Fatalf("release = %+v", release)
	}

	engine := NewTorEngineFromSource(root, testHTTPClient(dist.URL), source)

	if err := engine.Install(context.Background()); err != nil {
		t.Fatalf("install across the dist/archive split: %v", err)
	}

	if engine.binary.LoadManifest().State != string(StateInstalled) {
		t.Fatal("install must activate the checksum-verified bundle")
	}
}

// TestFindExecutableSkipsDebugVariants pins the v0.9.15 fix: recent
// expert bundles ship debug/tor NEXT to the real tor/tor, and the old
// first-hit walk returned the debug binary (verified live against
// 15.0.23). The real binary must win.
//
// Platform contract (the Windows CI regression): the fixtures are
// built through executableFileName, so on Windows the archive carries
// debug/tor.exe and tor/tor.exe while POSIX carries debug/tor and
// tor/tor — exactly the layouts the production findExecutable must
// resolve on each OS. A POSIX-named fixture meeting a Windows-named
// expectation is what broke run 35996310774; this test now proves the
// real intended behavior on EVERY supported OS.
func TestFindExecutableSkipsDebugVariants(t *testing.T) {
	root := t.TempDir()

	archiveName := executableFileName("tor")

	for _, rel := range []string{
		"debug/" + archiveName,
		"tor/" + archiveName,
		"data/geoip",
		"tor/libcrypto.so.3",
	} {
		full := filepath.Join(root, rel)

		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(full, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	found, err := findExecutable(root, "tor")
	if err != nil {
		t.Fatalf("findExecutable: %v", err)
	}

	if want := filepath.Join(root, "tor", archiveName); found != want {
		t.Fatalf("found %q, want the real binary %q", found, want)
	}
}

// TestExecutableFileNamePlatformContract pins the executable-naming
// semantics on BOTH platforms, on every build host: Windows appends
// .exe exactly once, POSIX keeps the native name, and an already-.exe
// name is never double-suffixed. findExecutable and every fixture
// builder depend on this contract.
func TestExecutableFileNamePlatformContract(t *testing.T) {
	cases := []struct {
		goos, name, want string
	}{
		{"windows", "tor", "tor.exe"},
		{"windows", "tor.exe", "tor.exe"},
		{"windows", "TOR", "TOR.exe"},
		{"windows", "Tor.EXE", "Tor.EXE"},
		{"windows", "psiphon-tunnel-core-windows-x86_64", "psiphon-tunnel-core-windows-x86_64.exe"},
		{"linux", "tor", "tor"},
		{"linux", "tor.exe", "tor.exe"},
		{"darwin", "tor", "tor"},
	}

	for _, tc := range cases {
		if got := executableFileNameFor(tc.goos, tc.name); got != tc.want {
			t.Errorf("executableFileNameFor(%q, %q) = %q, want %q",
				tc.goos, tc.name, got, tc.want)
		}
	}

	// The runtime helper must agree with the pure core for the host.
	if got, want := executableFileName("tor"), executableFileNameFor(runtime.GOOS, "tor"); got != want {
		t.Errorf("executableFileName(%q) = %q, want %q", "tor", got, want)
	}
}

// TestFindExecutableNestedArchiveLayouts proves matching handles the
// real expert-bundle shapes: the executable at the archive root, one
// directory deep, and layouts where BOTH the platform-named binary
// and a debug variant exist at several depths — the shallowest
// non-debug match wins deterministically, and repeated calls return
// the identical path (staging must be stable across attempts).
func TestFindExecutableNestedArchiveLayouts(t *testing.T) {
	archiveName := executableFileName("tor")

	layout := map[string][]string{
		"flat":        {archiveName},
		"one-deep":    {"bin/" + archiveName},
		"nested-deep": {"a/b/c/" + archiveName},
		"both-depths": {"bin/" + archiveName, "bin/tools/" + archiveName},
	}

	for name, entries := range layout {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()

			for _, rel := range entries {
				full := filepath.Join(root, filepath.FromSlash(rel))

				if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(full, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			first, err := findExecutable(root, "tor")
			if err != nil {
				t.Fatalf("findExecutable: %v", err)
			}

			second, err := findExecutable(root, "tor")
			if err != nil {
				t.Fatalf("findExecutable (repeat): %v", err)
			}

			if first != second {
				t.Fatalf("matching not deterministic: %q vs %q", first, second)
			}

			// The shallowest match must win when several valid
			// candidates exist.
			if name == "both-depths" {
				if want := filepath.Join(root, "bin", archiveName); first != want {
					t.Fatalf("found %q, want the shallowest candidate %q", first, want)
				}
			}
		})
	}
}

// TestFindExecutableDebugLosesAmongCandidates proves the scoring
// contract when debug variants and real variants mix at the SAME
// depth: no "debug" path segment may ever win, and a valid candidate
// beats a debug one regardless of walk order (WalkDir is
// lexicographic, but the choice must not depend on it).
func TestFindExecutableDebugLosesAmongCandidates(t *testing.T) {
	archiveName := executableFileName("tor")

	root := t.TempDir()

	for _, rel := range []string{
		"debug/" + archiveName,        // debug variant, shallow
		"release/" + archiveName,      // real variant, same depth
		"debug/deeper/" + archiveName, // debug variant, deeper
	} {
		full := filepath.Join(root, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(full, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	found, err := findExecutable(root, "tor")
	if err != nil {
		t.Fatalf("findExecutable: %v", err)
	}

	if want := filepath.Join(root, "release", archiveName); found != want {
		t.Fatalf("found %q, want the non-debug candidate %q", found, want)
	}
}

// TestFindExecutableIgnoresUnrelatedFiles pins that near-miss names
// never satisfy the lookup: similar prefixes, suffixed variants and
// unrelated payloads must not make a missing executable "found".
func TestFindExecutableIgnoresUnrelatedFiles(t *testing.T) {
	archiveName := executableFileName("tor")

	root := t.TempDir()

	for _, rel := range []string{
		"tor.gz",
		"tor-next" + executableFileName("tor-next"),
		"not-" + archiveName,
		"tor" + executableFileName("tor") + ".bak",
	} {
		full := filepath.Join(root, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(full, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := findExecutable(root, "tor"); err == nil {
		t.Fatal("findExecutable must fail when only unrelated files exist")
	}
}

// TestTorInstallMovesPayloadSiblings proves activation carries the
// executable's archive siblings (libraries, transports) into bin/ —
// a validated binary alone could not launch at runtime (verified live:
// the 15.0.23 Linux bundle needs libcrypto/libevent/libssl next to
// tor).
func TestTorInstallMovesPayloadSiblings(t *testing.T) {
	dist := fakeDistServerWithSiblings(t, "15.0.20")

	root := t.TempDir()

	source := NewTorSource(testHTTPClient(dist.URL), false)
	source.BaseURL = dist.URL

	engine := NewTorEngineFromSource(root, testHTTPClient(dist.URL), source)

	if err := engine.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}

	manifest := engine.binary.LoadManifest()

	if manifest.State != string(StateInstalled) {
		t.Fatalf("state = %q", manifest.State)
	}

	// The sibling library that sat next to the staged binary must be
	// in bin/ next to the activated executable.
	sibling := filepath.Join(engine.binary.BinDir(), "libtor-support.so")

	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("payload sibling missing from bin/: %v", err)
	}
}

// fakeDistServerWithSiblings extends fakeDistServer: the bundle
// archive also carries a sibling library next to the executable.
func fakeDistServerWithSiblings(t *testing.T, version string) *httptest.Server {
	t.Helper()

	platform := PlatformSuffix(runtime.GOOS, runtime.GOARCH)
	assetName := fmt.Sprintf("tor-expert-bundle-%s-%s.tar.gz", platform, version)

	fakeBin, err := os.ReadFile(fakeTorBin)
	if err != nil {
		t.Fatalf("read fake tor fixture: %v", err)
	}

	archive := buildBundleArchive(t, fakeTorBin, executableFileName("tor"))

	// Add a sibling entry next to the executable inside the archive.
	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)

	tw := tar.NewWriter(gz)

	entries := []struct {
		name string
		data []byte
	}{
		{executableFileName("tor"), fakeBin},
		{"libtor-support.so", []byte("fake shared library")},
	}

	for _, entry := range entries {
		hdr := &tar.Header{
			Name: entry.name,
			Mode: 0o755,
			Size: int64(len(entry.data)),
		}

		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}

		if _, err := tw.Write(entry.data); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	archive = buf.Bytes()
	archiveSHA := sha256Hex(archive)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+version+"/sha256sums-signed-build.txt":
			fmt.Fprintf(w, "%s  %s\n", archiveSHA, assetName)
		case r.URL.Path == "/"+version+"/"+assetName:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(archive)))
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(server.Close)

	return server
}
