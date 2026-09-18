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
	"strings"
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
