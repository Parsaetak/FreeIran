package app

// v0101_test.go — the crash-safe system-proxy recovery, proven at the
// composition-root level. A previous session that died while owning
// the system proxy (crash, kill, power loss) leaves a durable marker
// in <workspace>/runtime; the boot path must attempt the restoration
// BEFORE any service starts, and an unrecoverable marker must never
// block the application from starting.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/tunnel"
)

// staleProxyMarker is the exact durable shape tunnel.recoveryRecord
// writes (see engine/tunnel/recovery.go). Duplicated as a literal
// here on purpose: the marker is a cross-version, on-disk contract —
// a change in the tunnel struct that breaks this shape breaks old
// workspaces, and this test must notice.
const staleProxyMarker = `{
  "endpoint": "127.0.0.1:10808",
  "enabled_at_ms": 1758000000000,
  "previous": {
    "enabled": true,
    "server": "proxy.corp.example:8080",
    "bypass": ["localhost"],
    "saved": true
  }
}`

func TestAppBootsAfterCrashedProxySession(t *testing.T) {
	base := filepath.Join(t.TempDir(), "freeiran")

	runtimeDir := filepath.Join(base, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	marker := filepath.Join(runtimeDir, "system-proxy.json")
	if err := os.WriteFile(marker, []byte(staleProxyMarker), 0o600); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	application, err := New(Options{
		BaseDir:                 base,
		RefreshInterval:         time.Hour,
		RunIngestionOnStart:     false,
		SkipDefaultSources:      true,
		SkipConnectVerification: true,
	})
	if err != nil {
		t.Fatalf("boot with a crashed-session proxy marker: %v", err)
	}

	t.Cleanup(application.Shutdown)

	// A stale marker must never block startup: the application reaches
	// ready either way. The restoration outcome is platform-dependent
	// (WinINet restores on Windows; other platforms report the honest
	// unsupported state and keep the marker for the next boot).
	if state := application.State(); state.Status != "ready" {
		t.Fatalf("status = %s, want ready (a stale proxy marker must not block boot)", state.Status)
	}

	_, statErr := os.Stat(marker)

	switch runtime.GOOS {
	case "windows":
		// The WinINet backend restored the recorded previous state
		// and consumed the marker.
		if !os.IsNotExist(statErr) {
			t.Fatalf("marker still present after a successful Windows recovery: %v", statErr)
		}
	default:
		// The honest non-Windows behavior: the restore is reported
		// unsupported and the marker is KEPT for the next boot (the
		// machine state itself is a no-op on these platforms).
		if statErr != nil {
			t.Fatalf("marker must be retained when the platform cannot restore: %v", statErr)
		}
	}
}

func TestAppBootsWithoutProxyResidue(t *testing.T) {
	// The no-residue boot: no marker, no recovery work, ready.
	application, err := New(Options{
		BaseDir:                 filepath.Join(t.TempDir(), "freeiran"),
		RefreshInterval:         time.Hour,
		RunIngestionOnStart:     false,
		SkipDefaultSources:      true,
		SkipConnectVerification: true,
	})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}

	t.Cleanup(application.Shutdown)

	if state := application.State(); state.Status != "ready" {
		t.Fatalf("status = %s, want ready", state.Status)
	}

	if path := tunnel.RecoveryMarkerPath(); path != "" {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("fresh boot must not leave a proxy marker at %s: %v", path, err)
		}
	}
}
