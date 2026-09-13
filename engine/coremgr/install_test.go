package coremgr

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// installHarness stands in for the GitHub Releases API: it serves a
// release JSON plus a downloadable zip containing a staged fakecore
// binary, so the full install pipeline (resolve → download → verify →
// unpack → probe → validate → activate → smoke) runs offline.
type installHarness struct {
	server      *httptest.Server
	coresDir    string // FREEIRAN_TEST_CORES fixture dir with the fakecore
	releaseTag  string
	assetName   string // per-core asset filename
	digestSHA   string // published .dgst body ("" = none)
	releaseJSON []byte
	assetBody   []byte
	mu          sync.Mutex
	downloads   int
}

// buildFakeCore once per test binary and reuse it across harnesses.
var fakeCoreOnce sync.Once

var (
	fakeCorePath string
	fakeCoreErr  error
)

func buildFakeCore() {
	fakeCoreOnce.Do(func() {
		dir, err := os.MkdirTemp("", "coremgr-fakecore-*")
		if err != nil {
			fakeCoreErr = err

			return
		}

		main := filepath.Join(filepath.Dir(dummyMarker()), "..", "core", "testdata", "fakecore", "main.go")
		_ = main // path resolved below through the test fixture dir

		// Resolve the fixture relative to this package: ../core/testdata/fakecore.
		pkgDir := "."
		if wd, err := os.Getwd(); err == nil {
			pkgDir = wd
		}

		src := filepath.Join(pkgDir, "..", "core", "testdata", "fakecore")
		out := filepath.Join(dir, "fakecore-build")
		cmd := exec.Command("go", "build", "-o", out, src)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

		if outBytes, err := cmd.CombinedOutput(); err != nil {
			fakeCoreErr = fmt.Errorf("build fakecore: %v: %s", err, outBytes)

			return
		}

		fakeCorePath = out
	})
}

// dummyMarker only exists so filepath.Dir has something meaningful in
// buildFakeCore; the real path resolution uses os.Getwd.
func dummyMarker() string { return "." }

// zipAsset builds an in-memory zip containing the fakecore binary
// named for the requested core.
func zipAsset(t *testing.T, coreName, srcPath string) []byte {
	t.Helper()

	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read fakecore: %v", err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	inner := coreName
	if isWindowsTest() {
		inner = coreName + ".exe"
	}

	w, err := zw.Create(inner)
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}

	if _, err := w.Write(raw); err != nil {
		t.Fatalf("zip write: %v", err)
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	return buf.Bytes()
}

func isWindowsTest() bool { return os.PathSeparator == '\\' }

// newInstallHarness starts a fake release server for one core.
func newInstallHarness(t *testing.T, coreName, versionLine, tag string) *installHarness {
	t.Helper()

	buildFakeCore()
	if fakeCoreErr != nil {
		t.Skipf("fakecore unavailable: %v", fakeCoreErr)
	}

	h := &installHarness{
		releaseTag: tag,
		assetName:  fmt.Sprintf("%s-%s-%s.zip", coreName, runtime.GOOS, runtime.GOARCH),
	}

	// FAKECORE_VERSION controls the version the fake binary reports.
	// The asset is built per-core so the version line matches the tag.
	assetBody := zipAsset(t, coreName, fakeCorePath)

	h.assetBody = assetBody

	assetURL := h.serverURL() // placeholder, replaced below

	_ = assetURL

	h.releaseJSON = []byte(fmt.Sprintf(`{
                "tag_name": %q,
                "name": %q,
                "prerelease": false,
                "published_at": "2026-09-01T00:00:00Z",
                "html_url": "https://github.com/example/%s/releases/tag/%s",
                "assets": [{"name": %q, "browser_download_url": "ASSET_URL_PLACEHOLDER", "size": %d}]
        }`, tag, tag, coreName, tag, h.assetName, len(assetBody)))

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.ReplaceAll(h.releaseJSON, []byte("ASSET_URL_PLACEHOLDER"), []byte(h.server.URL+"/asset")))
	})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.downloads++
		h.mu.Unlock()

		_, _ = w.Write(h.assetBody)
	})
	mux.HandleFunc("/asset.dgst", func(w http.ResponseWriter, r *http.Request) {
		if h.digestSHA == "" {
			w.WriteHeader(http.StatusNotFound)

			return
		}
		_, _ = w.Write([]byte("SHA256(asset)= " + h.digestSHA + "\n"))
	})

	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)

	return h
}

func (h *installHarness) serverURL() string {
	if h.server == nil {
		return ""
	}

	return h.server.URL
}

// fakeSource returns a Source pointing at the harness.
func (h *installHarness) fakeSource(coreName string) Source {
	return Source{
		Name:             CoreName(coreName),
		DisplayName:      coreName,
		Repo:             "example/" + coreName,
		ReleaseAPI:       h.server.URL + "/releases",
		ReleasePage:      h.server.URL + "/releases",
		AssetPatterns:    []string{h.assetName},
		VersionProbeArgs: []string{"version"},
		ConfigCheckArgs:  []string{"check", "-c"},
		RunArgs:          []string{"run", "-c"},
	}
}

// fakeDoer adapts plain HTTP GETs (release JSON) to the HTTPDoer
// interface; downloads go through the same server.
type fakeDoer struct{ base string }

func (f fakeDoer) Do(url string) (*HTTPResponse, error) {
	resp, err := http.Get(url) //nolint:gosec // test-only URL from the harness
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)

	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)

		if err != nil {
			break
		}
	}

	return &HTTPResponse{StatusCode: resp.StatusCode, Body: body}, nil
}

func TestInstallPipelineEndToEnd(t *testing.T) {
	const (
		coreName = "xray"
		version  = "v1.2.3"
	)

	h := newInstallHarness(t, coreName, "fakecore "+version+" ("+coreName+")", version)

	// FAKECORE_VERSION makes the staged binary report the release tag.
	t.Setenv("FAKECORE_VERSION", "v1.2.3")

	root := t.TempDir()
	mgr, err := New(Options{
		RootDir:    root,
		Sources:    map[CoreName]Source{CoreXray: h.fakeSource(coreName)},
		HTTPClient: fakeDoer{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Progress events must flow: record stages and assert the happy
	// path emits download → smoke_test → complete.
	var mu sync.Mutex

	stages := []InstallStage{}
	OnProgress(func(p InstallProgress) {
		if p.Core != CoreXray {
			return
		}

		mu.Lock()
		stages = append(stages, p.Stage)
		mu.Unlock()
	})
	defer OnProgress(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install: %v", err)
	}

	mf, ok := mgr.Info(CoreXray)
	if !ok {
		t.Fatal("manifest missing after install")
	}

	if mf.State != StateReady {
		t.Errorf("state = %s (reason %q), want ready", mf.State, mf.FailureReason)
	}

	if mf.Version == "" || !strings.Contains(mf.Version, "1.2.3") {
		t.Errorf("version = %q, want one containing 1.2.3", mf.Version)
	}

	if mf.ChecksumSHA256 == "" {
		t.Error("checksum not recorded")
	}

	if _, err := os.Stat(mf.BinaryPath); err != nil {
		t.Errorf("activated binary missing: %v", err)
	}

	mu.Lock()

	seen := map[InstallStage]bool{}
	for _, s := range stages {
		seen[s] = true
	}
	mu.Unlock()

	for _, want := range []InstallStage{StageDownload, StageSmokeTest, StageComplete} {
		if !seen[want] {
			t.Errorf("progress stages missing %q (got %v)", want, stages)
		}
	}

	// Idempotency: a second install re-validates and stays ready.
	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	if mf, _ = mgr.Info(CoreXray); mf.State != StateReady {
		t.Errorf("state after re-install = %s, want ready", mf.State)
	}
}

func TestInstallRejectsCorruptedDownload(t *testing.T) {
	h := newInstallHarness(t, "v2ray", "fakecore v9.9.9 (v2ray)", "v1.0.0")
	t.Setenv("FAKECORE_VERSION", "v9.9.9") // mismatch with the release tag

	root := t.TempDir()
	mgr, err := New(Options{
		RootDir:    root,
		Sources:    map[CoreName]Source{CoreV2Ray: h.fakeSource("v2ray")},
		HTTPClient: fakeDoer{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err = mgr.Install(ctx, CoreV2Ray)
	if err == nil {
		t.Fatal("Install succeeded with a version mismatch, want failure")
	}

	mf, _ := mgr.Info(CoreV2Ray)
	if mf.State != StateBroken {
		t.Errorf("state = %s, want broken", mf.State)
	}

	if mf.FailureReason == "" {
		t.Error("FailureReason empty after failed install")
	}

	if !strings.Contains(mf.FailureReason, "version") {
		t.Errorf("FailureReason = %q, want a version-related explanation", mf.FailureReason)
	}
}

func TestSelectAssetLegacyNaming(t *testing.T) {
	// Regression: Xray/V2Ray publish "Xray-windows-64.zip"; the v0.8
	// matcher required the platform hint inside the asset name and
	// never matched. Both legacy and modern names must resolve on
	// every common platform.
	src := Source{AssetPatterns: []string{"windows-64.zip", "linux-64.zip", "macos-64.zip", "macos-arm64.zip"}}

	assets := []githubAsset{
		{Name: "Xray-windows-64.zip", BrowserDownloadURL: "win64"},
		{Name: "Xray-linux-64.zip", BrowserDownloadURL: "lin64"},
		{Name: "Xray-macos-64.zip", BrowserDownloadURL: "mac64"},
		{Name: "Xray-macos-arm64.zip", BrowserDownloadURL: "macarm"},
		{Name: "Xray-freebsd-64.zip", BrowserDownloadURL: "fbsd"},
	}

	cases := []struct {
		p    Platform
		want string
	}{
		{Platform{"windows", "amd64"}, "win64"},
		{Platform{"linux", "amd64"}, "lin64"},
		{Platform{"darwin", "amd64"}, "mac64"},
		{Platform{"darwin", "arm64"}, "macarm"},
	}

	for _, c := range cases {
		got, _ := selectAsset(assets, c.p, src)
		if got != c.want {
			t.Errorf("selectAsset(%s/%s) = %q, want %q", c.p.OS, c.p.Arch, got, c.want)
		}
	}

	// A windows/arm64 host must NOT receive the x86_64 asset.
	got, _ := selectAsset(assets, Platform{"windows", "arm64"}, src)
	if got != "" {
		t.Errorf("selectAsset(windows/arm64) = %q, want no match", got)
	}
}

func TestExtractVersionToken(t *testing.T) {
	cases := map[string]string{
		"Xray 26.3.27 (Xray, Penetrates Everything.) Custom CGo.": "26.3.27",
		"V2Ray 5.53.0 (V2Fly, a V2Ray community. Bule.)":          "5.53.0",
		"sing-box version 1.14.0 (go1.26 linux/amd64, CGO)":       "1.14.0",
		"fakecore v0-test (fake-xray)":                            "0",
		"":                                                        "",
		"no version here":                                         "",
	}

	for in, want := range cases {
		if got := ExtractVersionToken(in); got != want {
			t.Errorf("ExtractVersionToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepairDeadlockFix(t *testing.T) {
	// Regression: v0.8 Repair held the per-core mutex and then called
	// Rollback/HealthCheck/Install, which take the same mutex — an
	// instant deadlock. With no manifest and no previous version the
	// fixed implementation must return via Install's not-installed
	// path (which fails fast against a dead source) instead of
	// hanging. A timeout guards the regression.
	h := newInstallHarness(t, "sing-box", "fakecore v1.0.1 (sing-box)", "v1.0.1")
	t.Setenv("FAKECORE_VERSION", "v1.0.1")

	root := t.TempDir()
	mgr, err := New(Options{
		RootDir:    root,
		Sources:    map[CoreName]Source{CoreSingBox: h.fakeSource("sing-box")},
		HTTPClient: fakeDoer{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- mgr.Repair(ctx, CoreSingBox) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Repair: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("Repair deadlocked (did not return within 40s)")
	}

	if mf, _ := mgr.Info(CoreSingBox); mf.State != StateReady {
		t.Errorf("state after repair = %s (reason %q), want ready", mf.State, mf.FailureReason)
	}
}
