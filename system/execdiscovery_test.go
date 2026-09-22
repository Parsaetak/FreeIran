package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestDiscovery builds an ExecDiscovery with a counting fake probe
// and a fake PATH lookup, so discovery behaviour is exercised without
// spawning real processes.
func newTestDiscovery(t *testing.T) (*ExecDiscovery, *int64) {
	t.Helper()

	var probes int64

	d := NewExecDiscovery(func(ctx context.Context, path string) string {
		atomic.AddInt64(&probes, 1)

		// Deterministic fake probe: a "broken" path never answers;
		// otherwise version = the file stem.
		if strings.Contains(path, "broken") {
			return ""
		}

		base := filepath.Base(path)
		if idx := strings.Index(base, "."); idx > 0 && strings.HasSuffix(base, ".exe") {
			base = base[:idx]
		}

		return base + " 1.2.3"
	}, nil)

	// A known broken PATH lookup: tests install engines explicitly.
	d.lookPath = func(string) (string, error) {
		return "", os.ErrNotExist
	}

	return d, &probes
}

func TestExecDiscoveryManagedDirectory(t *testing.T) {
	d, probes := newTestDiscovery(t)

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	cand, err := d.Discover(context.Background(), "xray")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	if cand.Path != path {
		t.Fatalf("path = %s, want %s", cand.Path, path)
	}

	if cand.Origin != OriginManaged || cand.Ownership != OwnershipManaged {
		t.Fatalf("origin/ownership = %s/%s, want managed/managed", cand.Origin, cand.Ownership)
	}

	if !cand.Validated() {
		t.Fatal("candidate should be validated")
	}

	if atomic.LoadInt64(probes) != 1 {
		t.Fatalf("probes = %d, want 1", atomic.LoadInt64(probes))
	}
}

func TestExecDiscoveryPathOrigin(t *testing.T) {
	d, _ := newTestDiscovery(t)

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("v2ray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.lookPath = func(name string) (string, error) {
		if name == "v2ray" {
			return path, nil
		}

		return "", os.ErrNotExist
	}

	cands := d.DiscoverCandidates(context.Background(), "v2ray")
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}

	if cands[0].Origin != OriginPath || cands[0].Ownership != OwnershipExternal {
		t.Fatalf("origin/ownership = %s/%s, want path/external", cands[0].Origin, cands[0].Ownership)
	}
}

func TestExecDiscoverySystemDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME-rooted system probe is unix-specific")
	}

	d, _ := newTestDiscovery(t)

	home := t.TempDir()
	t.Setenv("HOME", home)

	binDir := filepath.Join(home, ".local", "bin")

	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(binDir, executableName("sing-box"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	cands := d.DiscoverCandidates(context.Background(), "sing-box")
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}

	if cands[0].Origin != OriginSystem {
		t.Fatalf("origin = %s, want system", cands[0].Origin)
	}

	if cands[0].Ownership != OwnershipExternal {
		t.Fatalf("ownership = %s, want external", cands[0].Ownership)
	}
}

func TestExecDiscoveryMultipleCandidatesOrdered(t *testing.T) {
	d, _ := newTestDiscovery(t)

	managed := t.TempDir()
	sysDir := t.TempDir()

	managedPath := filepath.Join(managed, executableName("xray"))
	sysPath := filepath.Join(sysDir, executableName("xray"))

	for _, p := range []string{managedPath, sysPath} {
		if err := os.WriteFile(p, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	d.AddManagedDir("xray", managed)
	d.lookPath = func(name string) (string, error) {
		if name == "xray" {
			return sysPath, nil
		}

		return "", os.ErrNotExist
	}

	cands := d.DiscoverCandidates(context.Background(), "xray")
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}

	if cands[0].Path != managedPath || cands[0].Origin != OriginManaged {
		t.Fatalf("best candidate = %s/%s, want managed first", cands[0].Path, cands[0].Origin)
	}

	if cands[1].Origin != OriginPath {
		t.Fatalf("second candidate origin = %s, want path", cands[1].Origin)
	}
}

func TestExecDiscoveryDuplicatePathsDeduplicated(t *testing.T) {
	d, _ := newTestDiscovery(t)

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	// The same directory registered twice, plus a PATH hit resolving
	// to the same file: the candidate list must deduplicate by path.
	d.AddManagedDir("xray", dir)
	d.AddManagedDir("xray", dir)
	d.lookPath = func(name string) (string, error) {
		return path, nil
	}

	cands := d.DiscoverCandidates(context.Background(), "xray")
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1 (deduplicated)", len(cands))
	}
}

func TestExecDiscoveryMissingExecutable(t *testing.T) {
	d, _ := newTestDiscovery(t)

	_, err := d.Discover(context.Background(), "xray")
	if err == nil {
		t.Fatal("missing engine should error")
	}

	if cands := d.DiscoverCandidates(context.Background(), "xray"); len(cands) != 0 {
		t.Fatalf("candidates = %d, want 0", len(cands))
	}
}

func TestExecDiscoveryInvalidExecutableStillListed(t *testing.T) {
	d, _ := newTestDiscovery(t)

	// The fake probe returns no version for paths containing
	// "broken" — a binary that exists but never answers a probe.
	dir := filepath.Join(t.TempDir(), "broken-install")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	cand, err := d.Discover(context.Background(), "xray")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	// An existing binary whose probe produces no version is reported
	// WITHOUT a version — the caller (registry) classifies it; a
	// broken binary is not silently treated as a missing one.
	if cand.Version != "" {
		t.Fatalf("version = %q, want empty", cand.Version)
	}

	if cand.Validated() {
		t.Fatal("broken binary must not be validated")
	}
}

func TestExecDiscoveryProbeTimeout(t *testing.T) {
	// A probe that never returns within its context budget must not
	// hang discovery: the fake probe honours ctx cancellation.
	d := NewExecDiscovery(func(ctx context.Context, path string) string {
		<-ctx.Done()

		return ""
	}, nil)

	d.lookPath = func(string) (string, error) { return "", os.ErrNotExist }

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()

	cand, err := d.Discover(ctx, "xray")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	if time.Since(start) > 5*time.Second {
		t.Fatal("probe timeout not honoured")
	}

	// The hung probe produced no version: candidate exists,
	// unvalidated, negative result cached with the short TTL.
	if cand.Version != "" {
		t.Fatalf("version = %q, want empty", cand.Version)
	}
}

func TestExecDiscoveryCachedReuse(t *testing.T) {
	d, probes := newTestDiscovery(t)

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	ctx := context.Background()

	for range 5 {
		if _, err := d.Discover(ctx, "xray"); err != nil {
			t.Fatalf("discover: %v", err)
		}
	}

	// Same unchanged binary: exactly ONE probe, the rest are cache
	// hits (no repeated process spawns for the same identity).
	if got := atomic.LoadInt64(probes); got != 1 {
		t.Fatalf("probes = %d, want 1 (cached reuse)", got)
	}
}

func TestExecDiscoveryFileIdentityChangeInvalidates(t *testing.T) {
	d, probes := newTestDiscovery(t)

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)
	d.SetFreshness(time.Hour, time.Hour) // exclude TTL expiry from this test

	ctx := context.Background()

	if _, err := d.Discover(ctx, "xray"); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// Replace the file: same path, different identity (size + mtime).
	newer := time.Now().Add(2 * time.Hour)

	if err := os.WriteFile(path, []byte("binary-v2-longer-content"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(path, newer, newer); err != nil {
		t.Fatal(err)
	}

	cand, err := d.Discover(ctx, "xray")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	if got := atomic.LoadInt64(probes); got != 2 {
		t.Fatalf("probes = %d, want 2 (identity change forces re-probe)", got)
	}

	if cand.ModTime.Equal(newer.Truncate(time.Second)) || cand.ModTime.Before(newer.Add(-time.Minute)) {
		t.Logf("modtime = %v (fs granularity tolerated)", cand.ModTime)
	}
}

func TestExecDiscoveryTTLExpiryReprobes(t *testing.T) {
	d, probes := newTestDiscovery(t)

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	ctx := context.Background()

	if _, err := d.Discover(ctx, "xray"); err != nil {
		t.Fatal(err)
	}

	// Age the cache entry past its bounded freshness window.
	d.mu.Lock()
	for _, entry := range d.cache {
		entry.probedAt = entry.probedAt.Add(-2 * DefaultProbeTTL)
	}
	d.mu.Unlock()

	if _, err := d.Discover(ctx, "xray"); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt64(probes); got != 2 {
		t.Fatalf("probes = %d, want 2 (TTL expiry forces re-probe)", got)
	}
}

func TestExecDiscoveryExplicitInvalidationBypassesCache(t *testing.T) {
	d, probes := newTestDiscovery(t)

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	ctx := context.Background()

	if _, err := d.Discover(ctx, "xray"); err != nil {
		t.Fatal(err)
	}

	// Explicit refresh (Verify) bypass: InvalidateAll forces the next
	// discovery to re-probe even though the identity is unchanged.
	d.InvalidateAll()

	if _, err := d.Discover(ctx, "xray"); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt64(probes); got != 2 {
		t.Fatalf("probes = %d, want 2 (explicit bypass)", got)
	}

	// Path-scoped invalidation behaves the same, precisely.
	d.InvalidatePath(path)

	if _, err := d.Discover(ctx, "xray"); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt64(probes); got != 3 {
		t.Fatalf("probes = %d, want 3 (path-scoped bypass)", got)
	}

	// Engine-scoped invalidation too.
	d.InvalidateEngine("xray")

	if _, err := d.Discover(ctx, "xray"); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt64(probes); got != 4 {
		t.Fatalf("probes = %d, want 4 (engine-scoped bypass)", got)
	}
}

func TestExecDiscoveryConcurrentProbesDeduplicated(t *testing.T) {
	d, probes := newTestDiscovery(t)

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	const callers = 16

	var wg sync.WaitGroup

	results := make([]string, callers)

	for i := range callers {
		wg.Add(1)

		go func(slot int) {
			defer wg.Done()

			cand, err := d.Discover(context.Background(), "xray")
			if err != nil {
				t.Errorf("discover: %v", err)

				return
			}

			results[slot] = cand.Version
		}(i)
	}

	wg.Wait()

	// Identical concurrent discovery calls share ONE probe.
	if got := atomic.LoadInt64(probes); got != 1 {
		t.Fatalf("probes = %d, want 1 (in-flight dedup)", got)
	}

	for i, v := range results {
		if v != results[0] || v == "" {
			t.Fatalf("result[%d] = %q, want shared %q", i, v, results[0])
		}
	}
}

func TestExecDiscoveryVanishedPathNotServed(t *testing.T) {
	d, _ := newTestDiscovery(t)

	dir := t.TempDir()
	path := filepath.Join(dir, executableName("xray"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	ctx := context.Background()

	if _, err := d.Discover(ctx, "xray"); err != nil {
		t.Fatal(err)
	}

	// The binary disappears: the cached entry must NOT be served.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if cands := d.DiscoverCandidates(ctx, "xray"); len(cands) != 0 {
		t.Fatalf("candidates = %d, want 0 (vanished binary)", len(cands))
	}
}

func TestExecDiscoveryWindowsExeNames(t *testing.T) {
	// The Windows ".exe" behaviour: EngineSpec names gain the platform
	// executable suffix when candidate paths are generated.
	spec := EngineSpec{Name: "xray", BinaryNames: []string{"xray"}, Subdirs: []string{"Xray"}}

	names := spec.binaryNames()

	want := executableName("xray")
	if len(names) != 1 || names[0] != want {
		t.Fatalf("binaryNames = %v, want [%s]", names, want)
	}

	if runtime.GOOS == "windows" && !strings.HasSuffix(want, ".exe") {
		t.Fatalf("windows name = %q, want .exe suffix", want)
	}
}

func TestCoreLocatorOriginReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, executableName("fakecore"))

	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	loc := NewCoreLocator(dir)

	binary, err := loc.Discover(context.Background(), "fakecore")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	if binary.Origin != string(OriginManaged) {
		t.Fatalf("origin = %q, want %q", binary.Origin, OriginManaged)
	}

	if binary.Ownership != string(OwnershipManaged) {
		t.Fatalf("ownership = %q, want %q", binary.Ownership, OwnershipManaged)
	}
}

// ---------------------------------------------------------------------------
// v0.9.14 CI-hardening coverage for the bounded-concurrent probe path
// ---------------------------------------------------------------------------

// TestExecDiscoveryBoundedConcurrentProbesMatchSequential pins the
// bounded-parallelism probe path: with MANY distinct candidates the
// candidate list, ordering and per-candidate probe counts are exactly
// what the sequential implementation produced, while the wall clock
// benefits from the (capped) parallelism.
func TestExecDiscoveryBoundedConcurrentProbesMatchSequential(t *testing.T) {
	d, probes := newTestDiscovery(t)

	const candidates = 12

	dir := t.TempDir()

	for i := 0; i < candidates; i++ {
		sub := filepath.Join(dir, fmt.Sprintf("slot%02d", i))
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}

		path := filepath.Join(sub, executableName("xray"))
		if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}

		d.AddManagedDir("xray", sub)
	}

	got := d.DiscoverCandidates(context.Background(), "xray")

	if len(got) != candidates {
		t.Fatalf("candidates = %d, want %d", len(got), candidates)
	}

	// Exactly one probe per distinct candidate — no duplicated spawns
	// introduced by the worker pool.
	if n := atomic.LoadInt64(probes); n != candidates {
		t.Fatalf("probes = %d, want %d (one per distinct candidate)", n, candidates)
	}

	// Deterministic ordering: all managed; stable path order.
	for i := 1; i < len(got); i++ {
		if got[i-1].Path >= got[i].Path {
			t.Fatalf("path order not deterministic: %s before %s", got[i-1].Path, got[i].Path)
		}
	}
}

// TestExecDiscoveryCancelledContextSpawnsNoProbes proves a cancelled
// caller never contributes new process launches: the shared flights
// may finish for other callers, but a discovery entered with a dead
// context reports no candidates instead of spawning probes.
func TestExecDiscoveryCancelledContextSpawnsNoProbes(t *testing.T) {
	d, probes := newTestDiscovery(t)

	dir := t.TempDir()

	path := filepath.Join(dir, executableName("xray"))
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := d.DiscoverCandidates(ctx, "xray")

	if len(got) != 0 {
		t.Fatalf("candidates = %d, want 0 for a cancelled discovery", len(got))
	}

	if n := atomic.LoadInt64(probes); n != 0 {
		t.Fatalf("probes = %d, want 0 (cancelled caller must not spawn work)", n)
	}
}

// TestExecDiscoveryWarmCacheSpawnsNoProbes verifies the warm-cache
// contract after the concurrency change: a second discovery within the
// freshness window over unchanged identities performs ZERO process
// spawns (stat + map lookups only).
func TestExecDiscoveryWarmCacheSpawnsNoProbes(t *testing.T) {
	d, probes := newTestDiscovery(t)

	dir := t.TempDir()

	path := filepath.Join(dir, executableName("xray"))
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	d.AddManagedDir("xray", dir)

	first := d.DiscoverCandidates(context.Background(), "xray")
	if len(first) != 1 {
		t.Fatalf("first discovery = %d candidates, want 1", len(first))
	}

	if n := atomic.LoadInt64(probes); n != 1 {
		t.Fatalf("cold probes = %d, want 1", n)
	}

	second := d.DiscoverCandidates(context.Background(), "xray")
	if len(second) != 1 || second[0].Version != first[0].Version {
		t.Fatal("warm discovery must return the identical cached candidate")
	}

	if n := atomic.LoadInt64(probes); n != 1 {
		t.Fatalf("probes after warm discovery = %d, want 1 (no respawn on warm cache)", n)
	}
}
