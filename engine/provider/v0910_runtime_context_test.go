// v0910_runtime_context_test.go proves the v0.9.10 runtime-context
// separation inside the provider engines themselves: a Tor run
// started through a SHORT-LIVED operation context keeps its process
// alive after that context expires (the pre-0.9.10 engine bound the
// process to the caller's context through exec.CommandContext, so a
// provider started from the Cores page died the moment the service
// call returned), and Stop still terminates it deterministically.
package provider

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// procsRunningUnder reports how many live processes execute a binary
// anywhere under dir (Linux /proc scan; other platforms rely on the
// engine's own Running()/health evidence).
func procsRunningUnder(t *testing.T, dir string) int {
	t.Helper()

	if runtime.GOOS != "linux" {
		return -1 // unknown on this platform
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

// TestTorRunContextOutlivesOperationContext is the engine-level proof
// of the provider lifetime rule.
func TestTorRunContextOutlivesOperationContext(t *testing.T) {
	dist := fakeDistServer(t, "15.0.20")

	root := t.TempDir()

	source := NewTorSource(testHTTPClient(dist.URL), false)
	source.BaseURL = dist.URL

	engine := NewTorEngineFromSource(root, testHTTPClient(dist.URL), source)

	if err := engine.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := engine.SetOptions(TorOptions{BootstrapTimeout: 30 * time.Second}); err != nil {
		t.Fatalf("options: %v", err)
	}

	// The operation context dies 400 ms after the start returns —
	// exactly what a service-layer `defer cancel()` does.
	opCtx, cancelOp := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancelOp()

	if err := engine.Start(opCtx); err != nil {
		t.Fatalf("start: %v", err)
	}

	if engine.State() != StateReady {
		t.Fatalf("state = %q, want ready", engine.State())
	}

	// Let the operation context expire and give any (wrong)
	// exec.CommandContext binding ample time to kill the process.
	<-opCtx.Done()
	time.Sleep(750 * time.Millisecond)

	health := engine.Health(context.Background())
	if !health.ProcessAlive {
		t.Fatal("the tor process died with its operation context — the pre-0.9.10 defect")
	}

	if running := procsRunningUnder(t, root); running >= 0 && running == 0 {
		t.Fatal("no live process executes the staged tor binary after the operation context expired")
	}

	// Stop is still the deterministic terminator.
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if engine.State() != StateInstalled {
		t.Fatalf("post-stop state = %q, want installed", engine.State())
	}

	if running := procsRunningUnder(t, root); running >= 0 && running != 0 {
		t.Fatalf("processes still running after Stop: %d", running)
	}
}
