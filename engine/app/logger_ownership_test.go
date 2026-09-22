package app

// Global-logger ownership regression coverage (v0.9.14 repair pass):
//
// The process-global logger is a SHARED piece of state. The lifecycle
// contract pinned here:
//
//   - a failed boot BEFORE the global logger is installed leaves the
//     global slot untouched (whatever the caller installed stays);
//   - a failed boot AFTER installation must not leave logging.Global
//     pointing at the closed boot logger;
//   - a supplied (injected) logger is CLOSED by New on every failure
//     path — an open handle on the log file blocks Windows TempDir
//     cleanup (the v0.9.14 CI failure class);
//   - a successful boot installs the active logger as the global;
//   - Shutdown closes the logger and uninstalls it from the global
//     slot (ClearGlobalIfCurrent never clobbers a newer owner).

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

// openBootLogger opens a directly-owned test logger and registers its
// idempotent close. Every test here uses it so no scenario leaks the
// handle even on a fatal assertion.
func openBootLogger(t *testing.T, dir string) *logging.Logger {
	t.Helper()

	logger, err := logging.Open(logging.Options{
		Dir:     filepath.Join(dir, "logs"),
		Name:    "test.log",
		Profile: logging.ProfileNormal,
	})
	if err != nil {
		t.Fatalf("open test logger: %v", err)
	}

	t.Cleanup(func() { _ = logger.Close() })

	return logger
}

// TestBootFailureBeforeGlobalInstallLeavesGlobalUnchanged pins the
// early-failure contract: EnsureLayout fails before app.New reaches
// logging.SetGlobal, so the transaction must close the injected logger
// WITHOUT touching the global slot.
func TestBootFailureBeforeGlobalInstallLeavesGlobalUnchanged(t *testing.T) {
	prev := logging.Global()
	t.Cleanup(func() { logging.SetGlobal(prev) })

	logDir := t.TempDir()
	logger := openBootLogger(t, logDir)

	// Deliberately install a DIFFERENT global: a failed early boot must
	// not uninstall it (it does not belong to this boot transaction).
	sentinel := openBootLogger(t, t.TempDir())
	logging.SetGlobal(sentinel)

	// A FILE where the workspace root must be created: EnsureLayout
	// fails before any other subsystem starts.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	if _, err := New(Options{BaseDir: blocker, Logger: logger}); err == nil {
		t.Fatal("app.New must fail when the base dir is a file")
	}

	if now := logging.Global(); now != sentinel {
		t.Fatalf("global logger changed by a failed early boot: %v (want the sentinel)", now)
	}

	// The injected logger was closed by the failure path: removing the
	// log directory must succeed on every platform (the Windows CI
	// guarantee exercised directly).
	if err := os.RemoveAll(logDir); err != nil {
		t.Fatalf("log dir not removable after failed boot: %v", err)
	}
}

// TestBootFailureAfterGlobalInstallUninstallsGlobal pins the
// late-failure contract: the boot installed its logger as the global
// (SetGlobal precedes the store open), so the transaction must
// uninstall it — logging.Global must never reference a closed logger.
func TestBootFailureAfterGlobalInstallUninstallsGlobal(t *testing.T) {
	prev := logging.Global()
	t.Cleanup(func() { logging.SetGlobal(prev) })

	dir := t.TempDir()
	logger := openBootLogger(t, dir)

	// Force a failure AFTER logger creation and global installation by
	// pointing the store at a file in place of the data directory.
	dataPath := filepath.Join(dir, "data")
	if err := os.WriteFile(dataPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := New(Options{
		BaseDir: dir,
		Logger:  logger,
	}); err == nil {
		t.Fatal("New must fail when the store path is not a directory")
	}

	if now := logging.Global(); now == logger {
		t.Fatal("logging.Global still references the closed boot logger")
	}

	// The slot must be back to the pre-boot owner (unset in a clean
	// process — the boot uninstalled its own registration).
	if now := logging.Global(); now != prev {
		t.Fatalf("global logger = %v, want the pre-boot owner %v", now, prev)
	}
}

// TestFailedBootClosesInjectedLogger observes the handle release
// directly through /proc/self/fd (Linux): after a failed boot NO file
// descriptor may reference the log directory.
func TestFailedBootClosesInjectedLogger(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc/self/fd")
	}

	dir := t.TempDir()
	logger := openBootLogger(t, dir)

	dataPath := filepath.Join(dir, "data")
	if err := os.WriteFile(dataPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := New(Options{
		BaseDir: dir,
		Logger:  logger,
	}); err == nil {
		t.Fatal("New must fail when the store path is not a directory")
	}

	if leaks := listOpenFilesUnder(dir); len(leaks) > 0 {
		t.Fatalf("file handles leaked after failed boot: %v", leaks)
	}
}

// TestGlobalLoggerLifecycleAcrossBootAndShutdown pins the successful
// lifecycle: boot installs the active logger as the global; Shutdown
// closes it and uninstalls it from the global slot.
func TestGlobalLoggerLifecycleAcrossBootAndShutdown(t *testing.T) {
	prev := logging.Global()
	t.Cleanup(func() { logging.SetGlobal(prev) })

	dir := t.TempDir()
	logger := openBootLogger(t, dir)

	application, err := New(Options{
		BaseDir:                 filepath.Join(dir, "freeiran"),
		Logger:                  logger,
		RefreshInterval:         time.Hour,
		RunIngestionOnStart:     false,
		SkipDefaultSources:      true,
		SkipConnectVerification: true,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	if now := logging.Global(); now != logger {
		t.Fatal("successful boot must install the active logger as the global")
	}

	application.Shutdown()

	if now := logging.Global(); now == logger {
		t.Fatal("shutdown left the closed logger reachable through logging.Global")
	}
}
