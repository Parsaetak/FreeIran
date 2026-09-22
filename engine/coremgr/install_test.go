package coremgr

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// installHarness stands in for the GitHub Releases API over a real
// local HTTP server (with the REAL production httpx client in front,
// so the full control-plane and data-plane policy is exercised):
// release JSON (with ETag + digest), the asset (Range-aware, with
// failure injection) and the .dgst sidecar.
type installHarness struct {
	server     *httptest.Server
	coreName   string
	releaseTag string
	assetName  string

	mu sync.Mutex

	digestSHA string // published .dgst body ("" = none)
	apiDigest string // release-API digest field ("" = none)

	releaseJSON []byte
	assetBody   []byte

	releaseETag string

	// counters + captured request evidence
	apiHits        int
	assetHits      int
	assetRanges    []string
	apiIfNoneMatch []string

	// failure injection
	apiStatuses   []int // consumed before falling back to 200
	assetFailures int   // hijack the first N asset requests mid-body
	stallAsset    bool
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
func newInstallHarness(t *testing.T, coreName, tag string) *installHarness {
	t.Helper()

	buildFakeCore()
	if fakeCoreErr != nil {
		t.Skipf("fakecore unavailable: %v", fakeCoreErr)
	}

	h := &installHarness{
		releaseTag: tag,
		assetName:  fmt.Sprintf("%s-%s-%s.zip", coreName, runtime.GOOS, runtime.GOARCH),
	}

	h.assetBody = zipAsset(t, coreName, fakeCorePath)

	// v0.9.8.6: the install pipeline refuses assets without an
	// authoritative digest, so the standard harness publishes the
	// .dgst sidecar by default. Tests exercising the rejection path
	// clear digestSHA themselves.
	assetSum := sha256.Sum256(h.assetBody)
	h.digestSHA = hex.EncodeToString(assetSum[:])

	h.releaseJSON = []byte(fmt.Sprintf(`{
                "tag_name": %q,
                "name": %q,
                "prerelease": false,
                "published_at": "2026-09-01T00:00:00Z",
                "html_url": "https://github.com/example/%s/releases/tag/%s",
                "assets": [{
                        "name": %q,
                        "browser_download_url": "ASSET_URL_PLACEHOLDER",
                        "size": %d,
                        "digest": "DIGEST_PLACEHOLDER"
                }]
        }`, tag, tag, coreName, tag, h.assetName, len(h.assetBody)))

	mux := http.NewServeMux()

	// The routes mirror the GitHub layout the identity verifier
	// expects: /<owner>/<repo>/releases/latest and
	// /<owner>/<repo>/releases/download/<tag>/<asset>.
	//
	// v0.9.14: dispatch is resolved against the CURRENT h.releaseTag /
	// h.assetName on every request, so tests can publish a NEWER
	// release mid-test (setRelease) — exactly the update scenario the
	// reuse-first install path has to handle.
	mux.HandleFunc("/repos/example/"+coreName+"/releases/latest", h.serveRelease)

	// The /releases LIST endpoint (CheckForUpdates / prerelease
	// channel): the same document wrapped in a one-element array,
	// dispatched against the CURRENT tag.
	mux.HandleFunc("/repos/example/"+coreName+"/releases", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") == "" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		h.mu.Lock()
		h.apiHits++
		body := h.releaseJSON
		h.mu.Unlock()

		wrapped := append([]byte{'['}, append(body, ']')...)

		_, _ = w.Write(wrapped)
	})

	mux.HandleFunc("/repos/example/"+coreName+"/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		tag := h.releaseTag
		asset := h.assetName
		h.mu.Unlock()

		if strings.HasSuffix(r.URL.Path, "/releases/download/"+tag+"/"+asset+".dgst") {
			digest := h.digestSHA
			if digest == "" {
				w.WriteHeader(http.StatusNotFound)

				return
			}

			_, _ = w.Write([]byte("SHA256(asset)= " + digest + "\n"))

			return
		}

		if strings.HasSuffix(r.URL.Path, "/releases/download/"+tag+"/"+asset) {
			h.serveAsset(w, r)

			return
		}

		w.WriteHeader(http.StatusNotFound)
	})

	h.coreName = coreName

	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)

	return h
}

// snapshot atomically captures the harness counters (v0.9.14 reuse
// tests assert on multiple counters consistently).
func (h *installHarness) snapshot() (out struct {
	apiHits   int
	assetHits int
}) {
	h.mu.Lock()
	defer h.mu.Unlock()

	out.apiHits = h.apiHits
	out.assetHits = h.assetHits

	return out
}

// setRelease publishes a NEWER release mid-test: the release document,
// the download route and the digest route all follow the current tag,
// so the next Install call resolves and acquires the new version —
// the real "update available" scenario.
func (h *installHarness) setRelease(tag string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.releaseTag = tag
	h.releaseJSON = []byte(fmt.Sprintf(`{
		"tag_name": %q,
		"name": %q,
		"prerelease": false,
		"published_at": "2026-09-01T00:00:00Z",
		"html_url": "https://github.com/example/%s/releases/tag/%s",
		"assets": [{
			"name": %q,
			"browser_download_url": "ASSET_URL_PLACEHOLDER",
			"size": %d,
			"digest": "DIGEST_PLACEHOLDER"
		}]
	}`, tag, tag, h.coreName, tag, h.assetName, len(h.assetBody)))
}

// serveRelease writes the release document (with ETag/304 and the
// API digest field).
func (h *installHarness) serveRelease(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()

	h.apiHits++

	if inm := r.Header.Get("If-None-Match"); inm != "" {
		h.apiIfNoneMatch = append(h.apiIfNoneMatch, inm)
	}

	statuses := h.apiStatuses
	if len(statuses) > 0 {
		h.apiStatuses = statuses[1:] // consume ONE per request
	}

	etag := h.releaseETag

	apiDigest := h.apiDigest

	body := h.releaseJSON

	assetName := h.assetName

	releaseTag := h.releaseTag

	h.mu.Unlock()

	if len(statuses) > 0 {
		code := statuses[0]

		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "0")
		}

		w.WriteHeader(code)

		return
	}

	if etag != "" {
		if inm := r.Header.Get("If-None-Match"); inm == etag {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("ETag", etag)
	}

	// Digest VALUE: "sha256:<hex>" or the empty string (both valid
	// JSON values for the "digest" field).
	digestValue := ""
	if apiDigest != "" {
		digestValue = "sha256:" + apiDigest
	}

	w.Header().Set("Content-Type", "application/json")

	// The asset URL mirrors this request's owner/repo path so the
	// identity verifier sees the repository it expects.
	downloadPath := strings.Replace(r.URL.Path,
		"/releases/latest",
		"/releases/download/"+releaseTag+"/"+assetName, 1)

	assetURL := h.server.URL + downloadPath

	out := bytes.ReplaceAll(body, []byte("ASSET_URL_PLACEHOLDER"), []byte(assetURL))
	out = bytes.ReplaceAll(out, []byte("DIGEST_PLACEHOLDER"), []byte(digestValue))

	_, _ = w.Write(out)
}

// serveAsset serves the zip with byte-range support, failure
// injection and stall injection.
func (h *installHarness) serveAsset(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()

	h.assetHits++

	failures := h.assetFailures

	stall := h.stallAsset

	h.mu.Unlock()

	if failures > 0 {
		h.mu.Lock()
		h.assetFailures--
		h.mu.Unlock()

		// Write ~40% of the body, then reset the connection.
		cut := len(h.assetBody) * 2 / 5

		w.Header().Set("Content-Length", strconv.Itoa(len(h.assetBody)))
		_, _ = w.Write(h.assetBody[:cut])

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		hj, ok := w.(http.Hijacker)
		if !ok {
			t := w
			_ = t
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		conn, _, _ := hj.Hijack()
		_ = conn.Close()

		return
	}

	if stall {
		w.Header().Set("Content-Length", strconv.Itoa(len(h.assetBody)))
		_, _ = w.Write(h.assetBody[:1024])

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}

		return
	}

	// Range-aware serving for resume support.
	if rng := r.Header.Get("Range"); rng != "" {
		h.mu.Lock()
		h.assetRanges = append(h.assetRanges, rng)
		h.mu.Unlock()

		var start int64

		if _, err := fmt.Sscanf(strings.TrimPrefix(rng, "bytes="), "%d-", &start); err != nil ||
			start < 0 || start >= int64(len(h.assetBody)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)

			return
		}

		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, len(h.assetBody)-1, len(h.assetBody)))
		w.Header().Set("Content-Length", strconv.Itoa(len(h.assetBody)-int(start)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(h.assetBody[start:])

		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(h.assetBody)))
	_, _ = w.Write(h.assetBody)
}

// fakeSource returns a Source pointing at the harness. The Repo
// matches the URL layout the identity verifier expects.
func (h *installHarness) fakeSource(coreName string) Source {
	return Source{
		Name:             CoreName(coreName),
		DisplayName:      coreName,
		Repo:             "example/" + coreName,
		ReleaseAPI:       h.server.URL + "/repos/example/" + coreName + "/releases",
		ReleasePage:      h.server.URL + "/repos/example/" + coreName + "/releases",
		AssetPatterns:    []string{h.assetName},
		VersionProbeArgs: []string{"version"},
		ConfigCheckArgs:  []string{"check", "-c"},
		RunArgs:          []string{"run", "-c"},
	}
}

// testClient is the REAL production httpx client with a fast retry
// policy — the tests exercise the actual network path.
func testClient() *httpx.Client {
	return httpx.NewClient(httpx.Policy{
		RequestTimeout:    5 * time.Second,
		MaxRetries:        3,
		BackoffBase:       5 * time.Millisecond,
		BackoffMax:        20 * time.Millisecond,
		BackoffJitter:     0.1,
		MaxRetryAfterWait: 10 * time.Second,
		MaxBodyBytes:      4 << 20,
	})
}

// newTestManager builds a Manager bound to the harness.
func newTestManager(t *testing.T, h *installHarness, coreName string) *Manager {
	t.Helper()

	mgr, err := New(Options{
		RootDir:              t.TempDir(),
		Sources:              map[CoreName]Source{CoreName(coreName): h.fakeSource(coreName)},
		HTTPClient:           testClient(),
		DownloadStallTimeout: 1500 * time.Millisecond,
		DownloadMaxRetries:   2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return mgr
}

// ---------------------------------------------------------------------------
// End-to-end pipeline
// ---------------------------------------------------------------------------

// TestInstallPipelineEndToEnd verifies the full transactional pipeline
// and the unified stage lifecycle:
// resolving → downloading → verifying → unpacking → validating →
// activating → complete.
func TestInstallPipelineEndToEnd(t *testing.T) {
	const coreName = "xray"

	const tag = "v1.2.3"

	h := newInstallHarness(t, coreName, tag)

	t.Setenv("FAKECORE_VERSION", "v1.2.3")

	mgr := newTestManager(t, h, coreName)

	var mu sync.Mutex

	stages := []InstallStage{}

	var downloadEvents []InstallProgress

	OnProgress(func(p InstallProgress) {
		if p.Core != CoreXray {
			return
		}

		mu.Lock()
		defer mu.Unlock()

		if p.Stage == StageDownloading {
			downloadEvents = append(downloadEvents, p)

			return
		}

		stages = append(stages, p.Stage)
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

	// The unified lifecycle must be emitted in order. (Unlock
	// explicitly: the second Install below emits more events through
	// the same listener.)
	mu.Lock()

	want := []InstallStage{
		StageResolving, StageVerifying, StageUnpacking,
		StageValidating, StageActivating, StageComplete,
	}

	if len(stages) < len(want) {
		mu.Unlock()
		t.Fatalf("stages = %v, want at least %v", stages, want)
	}

	for _, w := range want {
		found := false

		for _, s := range stages {
			if s == w {
				found = true

				break
			}
		}

		if !found {
			t.Errorf("stage %q missing from %v", w, stages)
		}
	}

	// Download telemetry must carry real byte counters.
	if len(downloadEvents) == 0 {
		mu.Unlock()
		t.Fatal("no downloading progress events")
	}

	last := downloadEvents[len(downloadEvents)-1]

	if last.BytesTotal != int64(len(h.assetBody)) {
		t.Errorf("final download event total = %d, want %d", last.BytesTotal, len(h.assetBody))
	}

	mu.Unlock()

	// Idempotency: a second install re-validates and stays ready.
	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	if mf, _ = mgr.Info(CoreXray); mf.State != StateReady {
		t.Errorf("state after re-install = %s, want ready", mf.State)
	}
}

// TestInstallRejectsCorruptedDownload proves a version mismatch is
// caught BEFORE activation (the staged binary never reaches bin/).
func TestInstallRejectsCorruptedDownload(t *testing.T) {
	h := newInstallHarness(t, "v2ray", "v1.0.0")

	t.Setenv("FAKECORE_VERSION", "v9.9.9") // mismatch with the release tag

	mgr := newTestManager(t, h, "v2ray")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := mgr.Install(ctx, CoreV2Ray)
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

	// No binary may be active after a failed fresh install.
	if _, statErr := os.Stat(mf.BinaryPath); statErr == nil {
		t.Error("a binary was activated despite the failed install")
	}
}

// TestInstallChecksumMismatchViaAPIDigest proves the GitHub
// release-API digest field is the primary checksum authority.
func TestInstallChecksumMismatchViaAPIDigest(t *testing.T) {
	h := newInstallHarness(t, "xray", "v2.0.0")

	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	// A WRONG digest published by the release API.
	h.mu.Lock()
	h.apiDigest = strings.Repeat("ab", 32)
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := mgr.Install(ctx, CoreXray)
	if err == nil {
		t.Fatal("Install succeeded despite an API-digest mismatch")
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.FailureStage != "verify_digest" {
		t.Fatalf("failure stage = %q, want verify_digest", mf.FailureStage)
	}

	if !strings.Contains(mf.FailureReason, "Checksum mismatch") &&
		!strings.Contains(mf.FailureReason, "SHA-256") {
		t.Errorf("FailureReason = %q, want checksum-mismatch wording", mf.FailureReason)
	}
}

// TestInstallVerifiesAgainstPublishedSidecarDigest proves the .dgst
// sidecar verification path (Xray/V2Ray convention) when the API
// digest is absent.
func TestInstallVerifiesAgainstPublishedSidecarDigest(t *testing.T) {
	h := newInstallHarness(t, "xray", "v3.0.0")

	t.Setenv("FAKECORE_VERSION", "v3.0.0")

	sum := sha256.Sum256(h.assetBody)
	h.mu.Lock()
	h.digestSHA = hex.EncodeToString(sum[:])
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install with a correct sidecar digest: %v", err)
	}

	mf, _ := mgr.Info(CoreXray)
	if mf.State != StateReady {
		t.Fatalf("state = %s (reason %q)", mf.State, mf.FailureReason)
	}
}

// TestInstallRejectsMissingAuthoritativeDigest pins the v0.9.8.6
// executable-trust invariant: when a release publishes NO digest
// (neither the release-API digest field nor a .dgst sidecar), the
// install is REJECTED. A locally computed SHA-256 is tamper evidence,
// never a trust anchor — it may not make a remotely acquired
// executable runnable.
func TestInstallRejectsMissingAuthoritativeDigest(t *testing.T) {
	h := newInstallHarness(t, "xray", "v1.4.0")

	t.Setenv("FAKECORE_VERSION", "v1.4.0")

	// No authoritative digest anywhere: clear the harness default and
	// keep the API digest empty.
	h.mu.Lock()
	h.digestSHA = ""
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := mgr.Install(ctx, CoreXray)
	if err == nil {
		t.Fatal("Install succeeded without any authoritative digest, want rejection")
	}

	if !strings.Contains(err.Error(), "verify_digest") {
		t.Fatalf("err = %v, want a verify_digest failure", err)
	}

	// The structured wrap names the stage; the UNWRAPPED cause carries
	// the full explanation (fail() replaces the cause text with the
	// stage summary but preserves the chain for errors.Unwrap).
	var explanation string

	for cur := error(err); cur != nil; cur = errors.Unwrap(cur) {
		explanation += cur.Error() + " "
	}

	if !strings.Contains(explanation, "unverified executable") {
		t.Fatalf("unwrapped chain = %q, want the unverified-executable explanation", explanation)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.State != StateBroken {
		t.Errorf("state = %s, want broken", mf.State)
	}

	// No executable may be active after the rejection.
	if _, statErr := os.Stat(mf.BinaryPath); statErr == nil {
		t.Error("a binary was activated despite the missing digest")
	}
}

// TestInstallRetriesRelease429 proves the release-resolution failure
// mode is fixed: a rate-limited API is retried (honouring Retry-After)
// and the install then succeeds.
func TestInstallRetriesRelease429(t *testing.T) {
	h := newInstallHarness(t, "xray", "v4.0.0")

	t.Setenv("FAKECORE_VERSION", "v4.0.0")

	// Two 429s before the real document.
	h.mu.Lock()
	h.apiStatuses = []int{http.StatusTooManyRequests, http.StatusTooManyRequests}
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install: %v (429 should have been retried)", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.apiHits != 3 {
		t.Fatalf("API hits = %d, want 3 (two 429s + one 200)", h.apiHits)
	}

	if mf, _ := mgr.Info(CoreXray); mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}
}

// TestInstallReleaseAPIUnavailable proves a persistent release-API
// failure surfaces an actionable "release API unavailable" message
// instead of a misleading "no asset" error.
func TestInstallReleaseAPIUnavailable(t *testing.T) {
	h := newInstallHarness(t, "xray", "v5.0.0")

	// Exhaust the retry ladder with 503s.
	h.mu.Lock()
	h.apiStatuses = []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	}
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := mgr.Install(ctx, CoreXray)
	if err == nil {
		t.Fatal("Install succeeded, want release-API failure")
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.FailureStage != "resolve_release" {
		t.Fatalf("failure stage = %q, want resolve_release", mf.FailureStage)
	}

	if !strings.Contains(mf.FailureReason, "temporary error") &&
		!strings.Contains(mf.FailureReason, "unavailable") {
		t.Errorf("FailureReason = %q, want release-API-unavailable wording", mf.FailureReason)
	}
}

// TestInstallDownloadRetriesAndResumes proves a mid-body connection
// loss is retried and RESUMED with an HTTP Range request.
func TestInstallDownloadRetriesAndResumes(t *testing.T) {
	h := newInstallHarness(t, "xray", "v6.0.0")

	t.Setenv("FAKECORE_VERSION", "v6.0.0")

	h.mu.Lock()
	h.assetFailures = 1 // first asset request dies mid-body
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	var mu sync.Mutex

	var sawResumedMessage bool

	var lastProgress InstallProgress

	OnProgress(func(p InstallProgress) {
		if p.Core != CoreXray {
			return
		}

		mu.Lock()
		defer mu.Unlock()

		lastProgress = p

		if p.ResumedBytes > 0 || strings.Contains(p.Message, "resumed from") {
			sawResumedMessage = true
		}
	})
	defer OnProgress(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install: %v (download should have resumed)", err)
	}

	h.mu.Lock()
	ranges := append([]string(nil), h.assetRanges...)
	assetHits := h.assetHits
	h.mu.Unlock()

	if assetHits != 2 {
		t.Fatalf("asset hits = %d, want 2 (failed + resumed)", assetHits)
	}

	if len(ranges) == 0 || !strings.HasPrefix(ranges[0], "bytes=") {
		t.Fatalf("resume request missing Range header: %v", ranges)
	}

	mu.Lock()
	defer mu.Unlock()

	if !sawResumedMessage {
		t.Errorf("no resumed telemetry observed (last progress = %+v)", lastProgress)
	}

	if mf, _ := mgr.Info(CoreXray); mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}
}

// TestInstallStalledDownloadFailsCleanly proves a stalled asset
// server fails the install with the actionable stall message and
// leaves no partial activation.
func TestInstallStalledDownloadFailsCleanly(t *testing.T) {
	h := newInstallHarness(t, "xray", "v7.0.0")

	t.Setenv("FAKECORE_VERSION", "v7.0.0")

	h.mu.Lock()
	h.stallAsset = true
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := mgr.Install(ctx, CoreXray)
	if err == nil {
		t.Fatal("Install succeeded against a stalled server")
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.FailureStage != "download" {
		t.Fatalf("failure stage = %q, want download", mf.FailureStage)
	}

	if !strings.Contains(mf.FailureReason, "stalled") {
		t.Errorf("FailureReason = %q, want stall wording", mf.FailureReason)
	}

	if _, statErr := os.Stat(mf.BinaryPath); statErr == nil {
		t.Error("a binary was activated despite the stalled download")
	}
}

// TestInstallSmokeTestFailureKeepsPreviousCore proves the transaction:
// when an UPDATE's staged binary fails its smoke test, the PREVIOUS
// working binary stays active.
func TestInstallSmokeTestFailureKeepsPreviousCore(t *testing.T) {
	h := newInstallHarness(t, "xray", "v8.0.0")

	t.Setenv("FAKECORE_VERSION", "v8.0.0")

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// First install: healthy core active.
	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	before, _ := mgr.Info(CoreXray)
	beforeSum, _ := fileSHA256(before.BinaryPath)

	// Second install: an UPDATE to a newer release whose staged
	// binary fails its smoke run. (v0.9.14: re-installing the SAME
	// current version is a reuse no-op — the failing-update path is
	// exercised with a real version bump.)
	h.setRelease("v8.1.0")

	t.Setenv("FAKECORE_VERSION", "v8.1.0")
	t.Setenv("FAKECORE_FAIL_FAST", "1")

	if err := mgr.Install(ctx, CoreXray); err == nil {
		t.Fatal("second Install succeeded despite the smoke-test failure")
	}

	after, _ := mgr.Info(CoreXray)

	// The previous core is STILL the active binary, byte for byte.
	afterSum, err := fileSHA256(after.BinaryPath)
	if err != nil {
		t.Fatalf("previous binary vanished: %v", err)
	}

	if afterSum != beforeSum {
		t.Fatalf("active binary changed after a failed update: %s -> %s", beforeSum[:12], afterSum[:12])
	}

	// The core is NOT Broken: the previous version still works.
	if after.State != StateReady {
		t.Errorf("state = %s, want ready (previous healthy core preserved)", after.State)
	}

	// Staging is cleaned up.
	if _, statErr := os.Stat(mgr.StagingDir(CoreXray)); statErr == nil {
		t.Error("staging left behind after failed update")
	}
}

// TestInstallConcurrentSingleflight proves duplicate simultaneous
// installs of the same core are deduplicated: ONE download serves
// every concurrent caller.
func TestInstallConcurrentSingleflight(t *testing.T) {
	h := newInstallHarness(t, "xray", "v9.0.0")

	t.Setenv("FAKECORE_VERSION", "v9.0.0")

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const callers = 4

	errs := make([]error, callers)

	var wg sync.WaitGroup

	for i := 0; i < callers; i++ {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()
			errs[idx] = mgr.Install(ctx, CoreXray)
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Install #%d: %v", i, err)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.assetHits != 1 {
		t.Fatalf("asset downloads = %d, want 1 (singleflight)", h.assetHits)
	}

	if mf, _ := mgr.Info(CoreXray); mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}
}

// TestInstallWrongPlatformAssetRejected proves a wrong-platform asset
// is rejected by the identity verifier before any download.
func TestInstallWrongPlatformAssetRejected(t *testing.T) {
	h := newInstallHarness(t, "xray", "v10.0.0")

	t.Setenv("FAKECORE_VERSION", "v10.0.0")

	// Rewrite the release to publish a linux-only asset.
	otherOS := "linux"
	if runtime.GOOS == "linux" {
		otherOS = "windows"
	}

	h.mu.Lock()
	h.assetName = fmt.Sprintf("xray-%s-amd64.zip", otherOS)
	h.releaseJSON = []byte(fmt.Sprintf(`{
                "tag_name": "v10.0.0",
                "prerelease": false,
                "assets": [{"name": %q,
                        "browser_download_url": "ASSET_URL_PLACEHOLDER",
                        "size": %d}]
        }`, h.assetName, len(h.assetBody)))
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := mgr.Install(ctx, CoreXray)
	if err == nil {
		t.Fatal("Install succeeded with a wrong-platform asset")
	}

	h.mu.Lock()
	assetHits := h.assetHits
	h.mu.Unlock()

	if assetHits != 0 {
		t.Fatalf("asset downloads = %d, want 0 (rejected before download)", assetHits)
	}

	mf, _ := mgr.Info(CoreXray)

	if !strings.Contains(mf.FailureReason, "platform") &&
		!strings.Contains(mf.FailureReason, "architecture") &&
		!strings.Contains(mf.FailureReason, "build for this platform") {
		t.Errorf("FailureReason = %q, want wrong-platform wording", mf.FailureReason)
	}
}

// TestInstallCachedReleaseLookup proves the release-metadata cache:
// the first lookup stores the ETag, the second sends If-None-Match
// and reuses the 304-validated cached body.
func TestInstallCachedReleaseLookup(t *testing.T) {
	h := newInstallHarness(t, "xray", "v11.0.0")

	t.Setenv("FAKECORE_VERSION", "v11.0.0")

	h.mu.Lock()
	h.releaseETag = `"release-etag-1"`
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	h.mu.Lock()
	hitsAfterFirst := h.apiHits
	h.mu.Unlock()

	// Second install: conditional request must be sent.
	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.apiHits != hitsAfterFirst+1 {
		t.Fatalf("API hits grew by %d, want exactly 1 (conditional revalidation)",
			h.apiHits-hitsAfterFirst)
	}

	if len(h.apiIfNoneMatch) == 0 {
		t.Fatal("no If-None-Match header was sent on the second lookup")
	}

	if h.apiIfNoneMatch[len(h.apiIfNoneMatch)-1] != `"release-etag-1"` {
		t.Fatalf("If-None-Match = %v, want the stored ETag", h.apiIfNoneMatch)
	}
}

// TestInstallUntrustedAssetHostRejected proves an asset URL pointing
// outside the source authority / GitHub hosts is refused.
func TestInstallUntrustedAssetHostRejected(t *testing.T) {
	h := newInstallHarness(t, "xray", "v12.0.0")

	t.Setenv("FAKECORE_VERSION", "v12.0.0")

	// Keep the platform-matching asset NAME so selection succeeds
	// and the identity verifier is what rejects the foreign host.
	platformAsset := fmt.Sprintf("xray-%s-%s.zip", runtime.GOOS, runtime.GOARCH)

	h.mu.Lock()
	h.releaseJSON = []byte(fmt.Sprintf(`{
        "tag_name": "v12.0.0",
        "prerelease": false,
        "assets": [{"name": %q,
                "browser_download_url": "https://evil.example.com/%s",
                "size": %d}]
        }`, platformAsset, platformAsset, len(h.assetBody)))
	h.assetName = platformAsset
	h.mu.Unlock()

	mgr := newTestManager(t, h, "xray")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err == nil {
		t.Fatal("Install succeeded with an untrusted asset host")
	}

	mf, _ := mgr.Info(CoreXray)

	if !strings.Contains(mf.FailureReason, "unrecognized host") &&
		!strings.Contains(mf.FailureReason, "does not belong to repository") {
		t.Errorf("FailureReason = %q, want untrusted-host wording", mf.FailureReason)
	}
}

// TestSelectAssetLegacyNaming keeps the v0.9.0 regression coverage for
// the Xray/V2Ray legacy naming scheme.
func TestSelectAssetLegacyNaming(t *testing.T) {
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
		got, _, _, _ := selectAsset(assets, c.p, src)
		if got != c.want {
			t.Errorf("selectAsset(%s/%s) = %q, want %q", c.p.OS, c.p.Arch, got, c.want)
		}
	}

	// A windows/arm64 host must NOT receive the x86_64 asset.
	got, _, _, _ := selectAsset(assets, Platform{"windows", "arm64"}, src)
	if got != "" {
		t.Errorf("selectAsset(windows/arm64) = %q, want no match", got)
	}
}

// TestExtractVersionToken keeps the version-token extraction coverage.
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

// TestRepairDeadlockFix keeps the v0.9.0 deadlock regression coverage.
func TestRepairDeadlockFix(t *testing.T) {
	h := newInstallHarness(t, "sing-box", "v1.0.1")

	t.Setenv("FAKECORE_VERSION", "v1.0.1")

	mgr := newTestManager(t, h, "sing-box")

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

// TestDigestSHANormalization covers the GitHub digest-field parser.
func TestDigestSHANormalization(t *testing.T) {
	good := strings.Repeat("cd", 32)

	cases := map[string]string{
		"sha256:" + good:                  good,
		"SHA256:" + strings.ToUpper(good): good,
		"sha512:" + good + good:           "",
		"":                                "",
		"sha256:tooshort":                 "",
	}

	for in, want := range cases {
		if got := digestSHA(in); got != want {
			t.Errorf("digestSHA(%q) = %q, want %q", in, got, want)
		}
	}
}
