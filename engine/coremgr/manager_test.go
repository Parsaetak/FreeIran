package coremgr

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestDefaultSourcesCoverAllCores verifies that every core has a
// source definition (Xray, V2Ray, sing-box) — the install pipeline
// would fail at the first lookup otherwise.
func TestDefaultSourcesCoverAllCores(t *testing.T) {
	sources := DefaultSources()
	for _, name := range AllCores {
		src, ok := sources[name]
		if !ok {
			t.Fatalf("no source defined for core %s", name)
		}
		if src.Repo == "" {
			t.Fatalf("source for %s has no Repo", name)
		}
		if src.ReleaseAPI == "" {
			t.Fatalf("source for %s has no ReleaseAPI", name)
		}
		if len(src.AssetPatterns) == 0 {
			t.Fatalf("source for %s has no asset patterns", name)
		}
		if len(src.VersionProbeArgs) == 0 {
			t.Fatalf("source for %s has no version probe args", name)
		}
		if len(src.ConfigCheckArgs) == 0 {
			t.Fatalf("source for %s has no config check args", name)
		}
		if len(src.RunArgs) == 0 {
			t.Fatalf("source for %s has no run args", name)
		}
	}
}

// TestAssetPatternForPlatform verifies the asset selection heuristic
// picks the right pattern for common platforms.
func TestAssetPatternForPlatform(t *testing.T) {
	cases := []struct {
		os, arch, want string
	}{
		{"windows", "amd64", "windows-amd64"},
		{"windows", "arm64", "windows-arm64"},
		{"linux", "amd64", "linux-amd64"},
		{"linux", "arm64", "linux-arm64"},
		{"darwin", "amd64", "darwin-amd64"},
		{"darwin", "arm64", "darwin-arm64"},
	}
	for _, c := range cases {
		got := AssetPatternForPlatform(Platform{OS: c.os, Arch: c.arch})
		if got != c.want {
			t.Errorf("AssetPatternForPlatform(%s/%s) = %q, want %q", c.os, c.arch, got, c.want)
		}
	}
}

// TestCompareVersions verifies the semver-ish comparison.
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.10.0", "1.9.0", 1}, // numeric, not lexical
		{"v1.0.0", "1.0.0", 0}, // leading v stripped
		{"26.3.27", "26.3.28", -1},
		{"1.14.0", "1.10.0", 1},
		{"", "", 0},
	}
	for _, c := range cases {
		got := compareVersions(c.a, c.b)
		// Normalize: we only care about sign.
		if (got < 0 && c.want >= 0) || (got > 0 && c.want <= 0) || (got == 0 && c.want != 0) {
			t.Errorf("compareVersions(%q, %q) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
}

// TestStripV verifies leading v stripping.
func TestStripV(t *testing.T) {
	cases := map[string]string{
		"v1.0.0": "1.0.0",
		"V1.0.0": "1.0.0",
		"1.0.0":  "1.0.0",
		"":       "",
		"v":      "",
	}
	for in, want := range cases {
		got := stripV(in)
		if got != want {
			t.Errorf("stripV(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestManagerConstruct verifies that New creates the directory tree
// and an empty manifest set.
func TestManagerConstruct(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Options{RootDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.RootDir() != dir {
		t.Errorf("RootDir = %s, want %s", m.RootDir(), dir)
	}
	// All() returns 3 manifests (one per core), all NotInstalled.
	all := m.All()
	if len(all) != len(AllCores) {
		t.Fatalf("All() returned %d, want %d", len(all), len(AllCores))
	}
	for _, mf := range all {
		if mf.State != StateNotInstalled {
			t.Errorf("state for %s = %s, want %s", mf.Name, mf.State, StateNotInstalled)
		}
	}
}

// TestManagerSetChannel verifies channel switching persists to disk.
func TestManagerSetChannel(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{RootDir: dir})

	if err := m.SetChannel(CoreXray, ChannelPrerelease); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}

	// Reload the manager from the same dir to verify persistence.
	m2, _ := New(Options{RootDir: dir})
	mf, _ := m2.Info(CoreXray)
	if mf.Channel != ChannelPrerelease {
		t.Errorf("channel after reload = %s, want %s", mf.Channel, ChannelPrerelease)
	}
}

// TestManagerDisableEnable verifies the state transitions.
func TestManagerDisableEnable(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{RootDir: dir})

	if err := m.Disable(CoreV2Ray); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	mf, _ := m.Info(CoreV2Ray)
	if mf.State != StateDisabled {
		t.Fatalf("state after disable = %s, want %s", mf.State, StateDisabled)
	}

	// Enable on a non-installed core should set StateBroken (no binary).
	if err := m.Enable(nil, CoreV2Ray); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	mf, _ = m.Info(CoreV2Ray)
	if mf.State != StateBroken {
		t.Fatalf("state after enable-without-binary = %s, want %s", mf.State, StateBroken)
	}
}

// TestManagerConcurrentAccess verifies that concurrent reads (Info,
// All) and writes (SetChannel, Disable, Enable, setState) don't race.
// This is a regression test for the data race where manifestOrCreate
// mutated m.manifests without holding m.mu while Info read it under
// m.mu.RLock.
func TestManagerConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{RootDir: dir})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Readers: call Info + All in a tight loop.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				for _, name := range AllCores {
					_, _ = m.Info(name)
				}
				_ = m.All()
			}
		}()
	}

	// Writers: call SetChannel / Disable / Enable in a tight loop.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				for _, name := range AllCores {
					ch := ChannelStable
					if worker%2 == 0 {
						ch = ChannelPrerelease
					}
					_ = m.SetChannel(name, ch)
					_ = m.Disable(name)
					_ = m.Enable(nil, name)
				}
			}
		}(i)
	}

	wg.Wait()
}
