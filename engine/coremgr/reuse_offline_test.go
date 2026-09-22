package coremgr

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/system"
)

// ---------------------------------------------------------------------------
// v0.9.14 local-first reuse under restrictive networks
//
// The release authority is replaced by a server that emulates the
// hostile-network failure classes a user behind a national firewall
// experiences: connection refused (authority down), a black-holed
// listener (SYN accepted, response never sent) and HTTP 403
// (rate-limit/poisoning style rejection). The reuse phase must keep a
// healthy local runtime usable in EVERY class, never block longer
// than the enrichment budget, and never touch the network when the
// local evidence alone answers the reuse question.
// ---------------------------------------------------------------------------

// offlineHarness is a harness whose release authority is unreachable.
type offlineHarness struct {
	t *testing.T

	coreName string
	assetHit bool // set when anything downloads (must stay false)
}

// newOfflineManager builds a Manager whose release authority always
// fails with the given handler-backed server (or refuses connections
// when handler is nil). The fakecore stands in for the core binary.
func newOfflineManager(t *testing.T, serverURL string) *Manager {
	t.Helper()

	buildFakeCore()
	if fakeCoreErr != nil {
		t.Skipf("fakecore unavailable: %v", fakeCoreErr)
	}

	const coreName = "xray"

	src := Source{
		Name:             CoreName(coreName),
		DisplayName:      coreName,
		Repo:             "example/" + coreName,
		ReleaseAPI:       serverURL + "/repos/example/" + coreName + "/releases",
		ReleasePage:      serverURL + "/repos/example/" + coreName + "/releases",
		AssetPatterns:    []string{coreName + "-" + runtime.GOOS + "-" + runtime.GOARCH + ".zip"},
		VersionProbeArgs: []string{"version"},
		ConfigCheckArgs:  []string{"check", "-c"},
		RunArgs:          []string{"run", "-c"},
	}

	mgr, err := New(Options{
		RootDir:              t.TempDir(),
		Sources:              map[CoreName]Source{CoreName(coreName): src},
		HTTPClient:           testClient(),
		DownloadStallTimeout: 1500 * time.Millisecond,
		DownloadMaxRetries:   2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return mgr
}

// seedManagedBinary installs a fakecore copy into the manager's
// managed slot and records a healthy, identity-current manifest.
func seedManagedBinary(t *testing.T, mgr *Manager, version string) string {
	t.Helper()

	const coreName = "xray"

	binDir := filepath.Join(mgr.RootDir(), "cores", coreName, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	binPath := filepath.Join(binDir, executableName(coreName))

	raw, err := os.ReadFile(fakeCorePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(binPath, raw, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := mgr.updateManifest(CoreName(coreName), func(mf *Manifest) {
		mf.State = StateReady
		mf.Version = version
		mf.BinaryPath = binPath
		mf.Ownership = string(system.OwnershipManaged)
		mf.Origin = string(system.OriginManaged)
		recordIdentity(mf)
	}); err != nil {
		t.Fatal(err)
	}

	return binPath
}

// seedExternalBinary records a healthy EXTERNAL reference (the file
// itself lives outside the workspace and must never be touched).
func seedExternalBinary(t *testing.T, mgr *Manager, version, latestKnown string) string {
	t.Helper()

	const coreName = "xray"

	extPath := copyFakeExternal(t, filepath.Join(t.TempDir(), "tooling"), coreName)

	if err := mgr.updateManifest(CoreName(coreName), func(mf *Manifest) {
		mf.State = StateReady
		mf.Version = version
		mf.BinaryPath = extPath
		mf.ExternalPath = extPath
		mf.Ownership = string(system.OwnershipExternal)
		mf.Origin = string(system.OriginPath)
		mf.LatestKnown = latestKnown
		recordIdentity(mf)
	}); err != nil {
		t.Fatal(err)
	}

	return extPath
}

// unreachableServer emulates an authority that is DOWN (connection
// refused — the cleanest restrictive-network signature).
func unreachableServer(t *testing.T) string {
	t.Helper()

	return "http://127.0.0.1:1"
}

// blackholeServer emulates a poisoned/black-holed endpoint: the TCP
// connection is accepted but NOTHING is ever sent back and the
// connection never closes. The caller's deadline is the only escape —
// exactly the signature of DPI interference and corrupted routing.
func blackholeServer(t *testing.T) string {
	t.Helper()

	lst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener available: %v", err)
	}

	t.Cleanup(func() { _ = lst.Close() })

	go func() {
		held := make([]net.Conn, 0, 8)

		for {
			conn, err := lst.Accept()
			if err != nil {
				return
			}

			// Hold the connection open forever: never read, never
			// write, never close — the caller's deadline is the only
			// escape, exactly like DPI black-holing.
			held = append(held, conn)
		}
	}()

	return "http://" + lst.Addr().String()
}

func TestOfflineReuseHealthyManagedNoDownload(t *testing.T) {
	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	mgr := newOfflineManager(t, unreachableServer(t))

	binPath := seedManagedBinary(t, mgr, "v2.0.0")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install offline with healthy managed core: %v", err)
	}

	if elapsed := time.Since(start); elapsed > reuseResolveBudget+10*time.Second {
		t.Fatalf("offline reuse took %s; the enrichment resolve must fail fast", elapsed)
	}

	mf, ok := mgr.Info(CoreXray)
	if !ok {
		t.Fatal("manifest missing")
	}

	if mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}

	// Without a cached target the honest decision is managed-healthy:
	// kept, release check unavailable.
	if mf.LastDecision != string(DecisionManagedHealthy) &&
		mf.LastDecision != string(DecisionManagedCurrent) {
		t.Errorf("last_decision = %q, want managed-healthy (or managed-current when cached)", mf.LastDecision)
	}

	if _, err := os.Stat(binPath); err != nil {
		t.Error("managed binary was disturbed during offline reuse")
	}
}

func TestOfflineReuseOlderExternalRetained(t *testing.T) {
	t.Setenv("FAKECORE_VERSION", "v1.0.0")

	mgr := newOfflineManager(t, unreachableServer(t))

	extPath := seedExternalBinary(t, mgr, "v1.0.0", "2.0.0")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install offline with healthy external core: %v", err)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.LastDecision != string(DecisionSystemOlder) {
		t.Errorf("last_decision = %q, want system-older-but-working (retained offline)", mf.LastDecision)
	}

	if mf.State != StateUpdateAvailable {
		t.Errorf("state = %s, want update_available (update still surfaced from cached target)", mf.State)
	}

	if _, err := os.Stat(extPath); err != nil {
		t.Error("external binary was modified during offline reuse")
	}
}

func TestOfflineReuseNewerExternalNeverDowngraded(t *testing.T) {
	t.Setenv("FAKECORE_VERSION", "v3.0.0")

	mgr := newOfflineManager(t, unreachableServer(t))

	extPath := seedExternalBinary(t, mgr, "v3.0.0", "2.0.0")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install offline with newer external core: %v", err)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.LastDecision != string(DecisionSystemNewer) {
		t.Errorf("last_decision = %q, want system-newer (never downgrade)", mf.LastDecision)
	}

	if mf.State != StateReady {
		t.Errorf("state = %s, want ready", mf.State)
	}

	if _, err := os.Stat(extPath); err != nil {
		t.Error("external binary was modified during offline reuse")
	}
}

func TestOfflineReuseBlackholedAuthorityStillReuses(t *testing.T) {
	if testing.Short() {
		t.Skip("black-hole timing test skipped in -short mode")
	}

	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	mgr := newOfflineManager(t, blackholeServer(t))

	seedManagedBinary(t, mgr, "v2.0.0")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install against black-holed authority: %v", err)
	}

	// The reuse answer must be bounded by LOCAL work alone: the release
	// resolve is detached enrichment now, so the call returns in local
	// time (milliseconds) — the old inline enrichment wait (the full
	// reuseResolveBudget) would overshoot this bound deterministically.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("black-holed authority blocked local reuse for %s; the reuse answer must not wait on the network at all", elapsed)
	}

	// The manifest must record a usable, reused runtime.
	mf, ok := mgr.Info(CoreXray)
	if !ok {
		t.Fatal("manifest missing")
	}

	if mf.State != StateReady && mf.State != StateUpdateAvailable {
		t.Errorf("state = %s, want a healthy reused state", mf.State)
	}
}

func TestOfflineNothingLocalFailsHonestly(t *testing.T) {
	t.Setenv("FAKECORE_VERSION", "v2.0.0")
	t.Setenv("FAKECORE_FAIL_FAST", "1")

	mgr := newOfflineManager(t, unreachableServer(t))

	// An installed binary whose identity no longer matches (the file
	// was corrupted/replaced after install) and which fails its local
	// smoke validation (fail-fast fakecore): offline there is nothing
	// else to acquire, so the install must fail honestly instead of
	// pretending success.
	binPath := seedManagedBinary(t, mgr, "v2.0.0")

	// Corrupt the binary AFTER the manifest identity was recorded: the
	// stale identity forces revalidation instead of the identity-trust
	// fast path.
	if err := os.WriteFile(binPath, []byte("corrupted — not an executable"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err == nil {
		t.Fatal("Install succeeded offline with an invalid local binary; want honest failure")
	}
}

func TestOfflineExternalAdoptedWhenNoActiveBinary(t *testing.T) {
	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	mgr := newOfflineManager(t, unreachableServer(t))

	// A working external installation exists on "PATH" but the manager
	// has no manifest yet. Offline, discovery + adoption must still
	// find it — the v0.9.14 initial revision failed the whole call.
	extPath := copyFakeExternal(t, filepath.Join(t.TempDir(), "tooling"), "xray")
	mgr.SetLocator(newLocatorWithExternal(t, "xray", extPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install offline with discoverable external core: %v", err)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.Ownership != string(system.OwnershipExternal) {
		t.Errorf("ownership = %q, want external (adopted by reference)", mf.Ownership)
	}

	if mf.BinaryPath != extPath {
		t.Errorf("binary_path = %q, want the external path %q", mf.BinaryPath, extPath)
	}

	if _, err := os.Stat(extPath); err != nil {
		t.Error("external binary was modified by offline adoption")
	}
}

// The 403 class: GitHub rate limits or an interception proxy answers
// with an authoritative rejection. Reuse must keep the local runtime.
func TestForbiddenAuthorityStillReusesLocalRuntime(t *testing.T) {
	t.Setenv("FAKECORE_VERSION", "v2.0.0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	mgr := newOfflineManager(t, srv.URL)

	seedManagedBinary(t, mgr, "v2.0.0")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mgr.Install(ctx, CoreXray); err != nil {
		t.Fatalf("Install against 403 authority: %v", err)
	}

	mf, _ := mgr.Info(CoreXray)

	if mf.State != StateReady {
		t.Errorf("state = %s, want ready (403 must not invalidate the local runtime)", mf.State)
	}
}
