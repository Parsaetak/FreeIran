// lifecycle_fakecore_test.go activates the two DORMANT fakecore
// failure injections (v0.9.8.7 regression coverage; the env vars
// existed in engine/core/testdata/fakecore since their introduction
// but no test exercised them):
//
//  1. FAKECORE_HANG         — a core that never becomes ready: the
//     startup-timeout path must force-stop the process, release the
//     image file (Windows: deletable) and remove the temporary
//     runtime directory — nothing may outlive the failed attempt.
//
//  2. FAKECORE_CRASH_AFTER_START — a core that dies mid-session: the
//     monitor must transition the session to connection_failed and
//     deterministically close the instance (process reaped, runtime
//     files removed). This is the crash-transition path that the
//     v0.9.8.5 change guarded with instance.Close().
//
// Both tests assert the Windows-critical file-lifecycle guarantee
// (executable deletable + directory removable after teardown). The
// process-existence proof uses the Linux /proc exe scan; on Windows
// the deletability assertions ARE the proof (a running image file
// cannot be removed there), so the scan is platform-guarded.
package connection_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
	"github.com/Parsaetak/FreeIran/system"
)

// hungCoreEnv builds the shared harness for the fakecore failure
// injections: a self-managed staging directory (so deletability is
// explicit evidence) plus a registry with the fake core staged.
func hungCoreEnv(t *testing.T) (dir, exePath string, registry *core.Registry) {
	t.Helper()

	// Self-managed staging: the test proves the directory becomes
	// removable AFTER teardown (t.TempDir would hide a leak).
	var err error

	dir, err = os.MkdirTemp("", "freeiran-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}

	exePath = contract.StageFakeCore(t, dir, "v2ray")

	registry = core.NewRegistry(system.NewCoreLocator(dir))

	if err := registry.Register(v2ray.New(), 1); err != nil {
		t.Fatalf("register v2ray: %v", err)
	}

	if err := registry.Register(xray.New(), 0); err != nil {
		t.Fatalf("register xray: %v", err)
	}

	registry.Refresh(context.Background())

	return dir, exePath, registry
}

// procsRunningFrom reports how many live processes execute a binary
// under dir (Linux /proc/<pid>/exe scan; bounded polling by the
// caller). Non-Linux platforms return 0 — there the file-lock
// deletability assertions below carry the proof.
func procsRunningFrom(t *testing.T, dir string) int {
	t.Helper()

	if runtime.GOOS != "linux" {
		return 0
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}

	count := 0

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()

		if name[0] < '0' || name[0] > '9' {
			continue
		}

		if target, err := os.Readlink(filepath.Join("/proc", name, "exe")); err == nil &&
			strings.HasPrefix(target, abs+string(filepath.Separator)) {
			count++
		}
	}

	return count
}

// runConfigLeftovers reports leftover freeiran runconfig directories
// under the system temp root created AFTER `before` was captured —
// the deterministic "temporary workspace was cleaned" evidence.
func runConfigLeftovers(t *testing.T, before map[string]bool) []string {
	t.Helper()

	tmp := os.TempDir()

	entries, err := os.ReadDir(tmp)
	if err != nil {
		return nil
	}

	var leftovers []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()

		if strings.HasPrefix(name, "freeiran-v2ray-") && !before[name] {
			leftovers = append(leftovers, filepath.Join(tmp, name))
		}
	}

	return leftovers
}

// existingRunConfigs snapshots the freeiran-v2ray-* directories that
// already exist in the temp root (other tests may legitimately have
// live run configs; only NEW leftovers are evidence of a leak).
func existingRunConfigs(t *testing.T) map[string]bool {
	t.Helper()

	before := map[string]bool{}

	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return before
	}

	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "freeiran-v2ray-") {
			before[entry.Name()] = true
		}
	}

	return before
}

// TestHungCoreStartupTimeoutForcesStopAndCleansFiles covers scenario
// (c) "forced kill" at the connection layer:
//
//	connect → core never becomes ready → startup timeout →
//	forced stop → process gone → executable deletable →
//	temporary runtime directory removed → connection_failed
func TestHungCoreStartupTimeoutForcesStopAndCleansFiles(t *testing.T) {
	t.Setenv("FAKECORE_HANG", "1")

	dir, exePath, registry := hungCoreEnv(t)

	before := existingRunConfigs(t)

	manager := connection.New(connection.Options{
		Registry:        registry,
		StartupTimeout:  2 * time.Second,
		GracePeriod:     time.Second,
		MonitorInterval: 200 * time.Millisecond,
		Verify:          connection.VerifyPolicy{Skip: true},
	})

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err == nil {
		manager.Shutdown()
		t.Fatalf("Connect() with a hung core succeeded (state %s) — the startup gate is broken", snapshot.State)
	}

	if snapshot.State != connection.StateConnectionFailed {
		t.Fatalf("state = %s, want connection_failed", snapshot.State)
	}

	// The failed session owns no process and no instance.
	if snapshot.CorePID != 0 {
		t.Fatalf("failed snapshot carries core_pid %d, want 0", snapshot.CorePID)
	}

	// Bounded polling (OS-genuine async observation): every process
	// spawned from the staging directory must be gone.
	waitFor(t, 15*time.Second, "hung core process to be force-stopped", func() bool {
		return procsRunningFrom(t, dir) == 0
	})

	// The startup-timeout close must have removed the temporary
	// runtime workspace it created.
	if leftovers := runConfigLeftovers(t, before); len(leftovers) > 0 {
		t.Fatalf("temporary runtime directories survived the failed attempt: %v", leftovers)
	}

	// The Windows guarantee: the image file must be releasable (on
	// Windows a still-running or handle-locked executable cannot be
	// removed — exactly the v0.9.8.5 defect class).
	if err := os.Remove(exePath); err != nil {
		t.Fatalf("staged executable %s is not deletable after the startup-timeout teardown: %v", exePath, err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("staging directory %s is not removable after teardown: %v", dir, err)
	}

	manager.Shutdown()
}

// TestMidSessionCrashTransitionsAndCleansUp covers the crash-detection
// path of the health monitor:
//
//	connect → core binds and turns ready → core exits unexpectedly →
//	monitor observes the death → connection_failed → instance closed →
//	process reaped → executable deletable → temp workspace removed
func TestMidSessionCrashTransitionsAndCleansUp(t *testing.T) {
	t.Setenv("FAKECORE_CRASH_AFTER_START", "1")

	dir, exePath, registry := hungCoreEnv(t)

	before := existingRunConfigs(t)

	manager := connection.New(connection.Options{
		Registry:        registry,
		StartupTimeout:  10 * time.Second,
		GracePeriod:     time.Second,
		MonitorInterval: 100 * time.Millisecond,
		Verify:          connection.VerifyPolicy{Skip: true},
	})

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		// The crash may land within the readiness window on a heavily
		// loaded machine; in that case the attempt already failed
		// cleanly and the same lifecycle assertions apply.
		if snapshot.State != connection.StateConnectionFailed {
			t.Fatalf("state = %s after failed connect, want connection_failed", snapshot.State)
		}
	} else if snapshot.State != connection.StateConnected {
		t.Fatalf("state = %s, want connected before the crash", snapshot.State)
	}

	// The monitor (or the failed attempt's cleanup) must transition to
	// the terminal failed state.
	waitFor(t, 15*time.Second, "connection_failed after the mid-session crash", func() bool {
		return manager.State() == connection.StateConnectionFailed
	})

	final := manager.Snapshot()

	if final.CorePID != 0 {
		t.Fatalf("snapshot core_pid = %d after the crash transition, want 0", final.CorePID)
	}

	waitFor(t, 15*time.Second, "crashed core process to be reaped", func() bool {
		return procsRunningFrom(t, dir) == 0
	})

	if leftovers := runConfigLeftovers(t, before); len(leftovers) > 0 {
		t.Fatalf("temporary runtime directories survived the crash teardown: %v", leftovers)
	}

	if err := os.Remove(exePath); err != nil {
		t.Fatalf("staged executable %s is not deletable after the crash teardown: %v", exePath, err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("staging directory %s is not removable after the crash teardown: %v", dir, err)
	}

	manager.Shutdown()
}
