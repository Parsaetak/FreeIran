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
// v0.9.11: the process-existence oracle is GENUINELY CROSS-PLATFORM.
// The pre-0.9.11 oracle (procsRunningFrom) returned a FAKE 0 on
// every non-Linux platform while the new v0.9.10 tests required 1 —
// three false Windows failures (and a reconnect assertion that could
// only die at its timeout). The oracle now combines real evidence on
// every platform:
//
//   - kernel process liveness of the recorded pid
//     (system.ProcessAlive: OpenProcess/GetExitCodeProcess on
//     Windows, kill(0)/EPERM on Unix);
//   - the session's local listener still accepting TCP connections
//     (serving evidence, platform-neutral);
//   - Linux: the /proc/<pid>/exe image scan (kept);
//   - Windows: the running image's executable-file lock
//     (a live image cannot be deleted; deletability is the teardown
//     proof — the assertions are bounded polls, never skips).
package connection_test

import (
	"context"
	"net"
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

// sessionEvidence captures the ownership evidence of one live
// session: the recorded core process id and the local listener
// address the session serves.
type sessionEvidence struct {
	pid      int
	endpoint string
}

// sessionEvidenceFrom reads the evidence from a connection snapshot.
func sessionEvidenceFrom(t *testing.T, snapshot connection.Snapshot) sessionEvidence {
	t.Helper()

	if snapshot.CorePID <= 0 {
		t.Fatal("snapshot carries no core pid (cannot prove process ownership)")
	}

	if snapshot.Endpoint == "" {
		t.Fatal("snapshot carries no endpoint (cannot prove listener ownership)")
	}

	return sessionEvidence{pid: snapshot.CorePID, endpoint: snapshot.Endpoint}
}

// listenerServes reports whether the session's local listener still
// accepts TCP connections. This is real, platform-neutral SERVING
// evidence: a live process whose listener is gone is not a serving
// session, and a port squatter without the process is not the
// session either. The fake core accepts and closes immediately.
func listenerServes(endpoint string) bool {
	if endpoint == "" {
		return false
	}

	conn, err := net.DialTimeout("tcp", endpoint, 500*time.Millisecond)
	if err != nil {
		return false
	}

	_ = conn.Close()

	return true
}

// alive asserts — with real evidence on EVERY platform — that the
// session's core process is alive and still serving:
//
//   - every platform: the recorded pid is alive (kernel liveness)
//     AND the local listener accepts a connection;
//   - Linux: the /proc image scan still finds a process executing an
//     image under dir;
//   - Windows: the live image keeps its executable locked — the
//     removal probe must FAIL while the process is alive (if the
//     image is deletable while the pid reports alive, the oracle
//     itself is broken and the test fails loudly).
func (e sessionEvidence) alive(t *testing.T, dir, exePath string) bool {
	t.Helper()

	if !system.ProcessAlive(e.pid) {
		return false
	}

	if !listenerServes(e.endpoint) {
		return false
	}

	switch runtime.GOOS {
	case "windows":
		// A running image file cannot be removed on Windows.
		// The lock FAILING to lift is liveness evidence here;
		// it must never succeed while the pid is alive.
		if err := os.Remove(exePath); err == nil {
			t.Fatal("staged executable was deletable while the core pid reports alive: " +
				"the Windows image-lock oracle is broken")
		}

		return true

	case "linux":
		return procsRunningFrom(t, dir) >= 1

	default:
		// Other Unix-like hosts: pid liveness + listener.
		return true
	}
}

// deadWait polls (bounded) until the session's process is gone.
//
//   - every platform: the pid stops reporting alive;
//   - every platform unless keepListener: the listener stops
//     accepting (after a REPLACEMENT the old port may legitimately
//     serve the new session — pass keepListener there);
//   - Linux: the /proc image scan converges to wantProcs;
//   - Windows: the image-lock release is asserted by the teardown
//     paths (assertTeardownComplete); during replacement the staged
//     image stays locked by the new session.
func (e sessionEvidence) deadWait(t *testing.T, dir string, wantProcs int, keepListener bool) {
	t.Helper()

	waitFor(t, 15*time.Second, "the replaced core process to terminate", func() bool {
		if system.ProcessAlive(e.pid) {
			return false
		}

		if !keepListener && listenerServes(e.endpoint) {
			return false
		}

		if runtime.GOOS == "linux" {
			return procsRunningFrom(t, dir) == wantProcs
		}

		return true
	})
}

// assertTeardownComplete proves a TERMINATED session released every
// resource — the bounded cross-platform teardown condition:
//
//   - Windows: the staged executable becomes deletable again (a live
//     or handle-locked image cannot be removed — exactly the
//     v0.9.8.5 defect class). The removal probe runs inside the
//     bounded poll and the file is CONSUMED by the successful probe.
//   - Linux: the /proc image scan finds no process from dir.
//
// After this returns, the staging directory itself must be removable
// on every platform.
func assertTeardownComplete(t *testing.T, dir, exePath string) {
	t.Helper()

	waitFor(t, 15*time.Second, "the supervised process to be torn down and its image released", func() bool {
		if runtime.GOOS == "windows" {
			if err := os.Remove(exePath); err != nil {
				return false
			}

			return true
		}

		return procsRunningFrom(t, dir) == 0
	})

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("staging directory %s is not removable after teardown: %v", dir, err)
	}
}

// procsRunningFrom reports how many live processes execute a binary
// under dir via the Linux /proc/<pid>/exe scan (bounded polling by
// the caller).
//
// v0.9.11: this is LINUX-ONLY evidence BY CONTRACT. The pre-0.9.11
// version returned a fake 0 on every other platform — the root cause
// of the false Windows failures. Any non-Linux call fails the test
// loudly instead of silently lying; cross-platform callers use the
// sessionEvidence oracle and assertTeardownComplete.
func procsRunningFrom(t *testing.T, dir string) int {
	t.Helper()

	if runtime.GOOS != "linux" {
		t.Fatal("procsRunningFrom is Linux-only evidence; " +
			"use sessionEvidence / assertTeardownComplete cross-platform")
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

	// Bounded polling with REAL platform evidence: Linux /proc
	// image scan; Windows image-lock release (the running image
	// cannot be removed, so deletability is the proof).
	assertTeardownComplete(t, dir, exePath)

	// The startup-timeout close must have removed the temporary
	// runtime workspace it created.
	if leftovers := runConfigLeftovers(t, before); len(leftovers) > 0 {
		t.Fatalf("temporary runtime directories survived the failed attempt: %v", leftovers)
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
	} else {
		if snapshot.State != connection.StateConnected {
			t.Fatalf("state = %s, want connected before the crash", snapshot.State)
		}

		// The pre-crash session is a REAL owned process with a
		// serving listener — record its evidence so the crash
		// transition below is provable on every platform.
		ev := sessionEvidenceFrom(t, snapshot)

		if !ev.alive(t, dir, exePath) {
			t.Fatalf("session evidence broken before the crash: pid %d not alive or listener %s not serving",
				ev.pid, ev.endpoint)
		}
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

	if ctx := manager.SessionContext(); ctx != nil {
		t.Fatal("session context must be released when the session crashes")
	}

	assertTeardownComplete(t, dir, exePath)

	if leftovers := runConfigLeftovers(t, before); len(leftovers) > 0 {
		t.Fatalf("temporary runtime directories survived the crash teardown: %v", leftovers)
	}

	manager.Shutdown()
}
