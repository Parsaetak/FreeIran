package tunnel

// recovery_test.go — the crash-safe system-proxy restoration contract
// (v0.10.1). The WinINet backend itself is only exercised on Windows;
// these tests prove the MARKER lifecycle and the restore routing with
// a deterministic in-memory backend, on every platform CI runs on.
//
// What is pinned here:
//
//   - Enable(system proxy) persists the ownership marker carrying the
//     PREVIOUS state (the state a crashed session must restore).
//   - Disable removes the marker: a clean session leaves no residue.
//   - RecoverStaleProxy restores the recorded previous state and
//     consumes the marker.
//   - A restore failure KEEPS the marker (the next boot retries).
//   - A corrupt marker is removed and surfaced, never restored from.
//   - No marker / no configured path = honest no-op.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeProxyBackend is a deterministic SystemProxyBackend double: it
// records every call and returns configured errors. It models the
// Windows save-on-Enable semantics exactly — the part the recovery
// contract depends on.
type fakeProxyBackend struct {
	enableErr  error
	disableErr error
	restoreErr error

	enabled   bool
	server    string
	bypass    []string
	previous  SystemProxySnapshot
	savedOnce bool

	restores []SystemProxySnapshot
}

func (f *fakeProxyBackend) Enable(_ context.Context, host string, port int, _ bool, bypass []string) error {
	if f.enableErr != nil {
		return f.enableErr
	}

	f.previous = SystemProxySnapshot{
		Enabled: f.enabled,
		Server:  f.server,
		Bypass:  f.bypass,
	}
	f.savedOnce = true

	f.enabled = true
	f.server = host
	f.bypass = bypass

	return nil
}

func (f *fakeProxyBackend) Disable(_ context.Context) error {
	if f.disableErr != nil {
		return f.disableErr
	}

	f.enabled = false

	return nil
}

// Snapshot mirrors winINetBackend: after Enable it reports the SAVED
// previous state (that is what the marker must persist); before any
// Enable it reports the current state.
func (f *fakeProxyBackend) Snapshot() SystemProxySnapshot {
	if f.savedOnce {
		return f.previous
	}

	return SystemProxySnapshot{Enabled: f.enabled, Server: f.server, Bypass: f.bypass}
}

func (f *fakeProxyBackend) Restore(previous SystemProxySnapshot) error {
	if f.restoreErr != nil {
		return f.restoreErr
	}

	f.restores = append(f.restores, previous)
	f.enabled = previous.Enabled
	f.server = previous.Server
	f.bypass = previous.Bypass

	return nil
}

// withMarkerPath points the package marker at a temp file for the
// duration of one test, injects a fake recovery backend and restores
// both globals afterwards.
func withMarkerPath(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "system-proxy.json")

	prevPath := recoveryMarkerPath
	prevBackend := recoveryBackend
	recoveryMarkerPath = path
	t.Cleanup(func() {
		recoveryMarkerPath = prevPath
		recoveryBackend = prevBackend
	})

	return path
}

// withFakeRecoveryBackend injects the fake backend into the recovery
// path and returns it.
func withFakeRecoveryBackend(t *testing.T) *fakeProxyBackend {
	t.Helper()

	backend := &fakeProxyBackend{}
	prev := recoveryBackend
	recoveryBackend = func() SystemProxyBackend { return backend }
	t.Cleanup(func() { recoveryBackend = prev })

	return backend
}

// markerOnDisk decodes the marker at path (test helper — asserts the
// durable shape, not the in-memory state).
func markerOnDisk(t *testing.T, path string) (recoveryRecord, bool) {
	t.Helper()

	blob, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return recoveryRecord{}, false
	}

	if err != nil {
		t.Fatalf("read marker: %v", err)
	}

	var record recoveryRecord
	if err := json.Unmarshal(blob, &record); err != nil {
		t.Fatalf("marker is not valid JSON: %v", err)
	}

	return record, true
}

// TestRecoveryMarkerWrittenOnEnable proves the crash-ownership
// contract: a successful Enable(system proxy) persists a marker
// carrying the PREVIOUS proxy state (here: a real corporate proxy),
// so a crash before Disable still has exact restore data.
func TestRecoveryMarkerWrittenOnEnable(t *testing.T) {
	path := withMarkerPath(t)

	backend := &fakeProxyBackend{
		enabled: true,
		server:  "proxy.corp.example:8080",
		bypass:  []string{"localhost"},
	}

	c := NewWithProxyBackend(backend)

	if err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	record, ok := markerOnDisk(t, path)
	if !ok {
		t.Fatal("no ownership marker written after successful Enable")
	}

	if record.Endpoint != "127.0.0.1:10808" {
		t.Errorf("marker endpoint = %q, want 127.0.0.1:10808", record.Endpoint)
	}

	if record.EnabledAtMS <= 0 {
		t.Errorf("marker enabled_at_ms = %d, want a positive timestamp", record.EnabledAtMS)
	}

	if !record.Previous.Enabled || record.Previous.Server != "proxy.corp.example:8080" {
		t.Fatalf("marker previous = %+v, want the corporate proxy state the Enable replaced", record.Previous)
	}

	if len(record.Previous.Bypass) != 1 || record.Previous.Bypass[0] != "localhost" {
		t.Errorf("marker previous bypass = %v, want [localhost]", record.Previous.Bypass)
	}
}

// TestRecoveryMarkerClearedOnDisable proves a clean Disable leaves
// no residue: the next boot must NOT "restore" anything.
func TestRecoveryMarkerClearedOnDisable(t *testing.T) {
	path := withMarkerPath(t)

	backend := &fakeProxyBackend{}

	c := NewWithProxyBackend(backend)

	if err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	if _, ok := markerOnDisk(t, path); !ok {
		t.Fatal("marker missing after Enable (test precondition)")
	}

	if err := c.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	if _, ok := markerOnDisk(t, path); ok {
		t.Fatal("ownership marker survived a clean Disable")
	}
}

// TestRecoveryMarkerSurvivesFailedDisable proves a FAILED Disable
// keeps the marker: the system still carries FreeIran's proxy and
// the ownership evidence must stay durable for the next boot.
func TestRecoveryMarkerSurvivesFailedDisable(t *testing.T) {
	path := withMarkerPath(t)

	backend := &fakeProxyBackend{disableErr: errors.New("wininet refused")}

	c := NewWithProxyBackend(backend)

	if err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	if err := c.Disable(context.Background()); err == nil {
		t.Fatal("Disable must fail when the backend refuses")
	}

	if _, ok := markerOnDisk(t, path); !ok {
		t.Fatal("marker was cleared even though Disable failed")
	}
}

// TestRecoverStaleProxyRestoresRecordedState proves the boot path: a
// marker left by a crashed session is restored EXACTLY (the recorded
// previous state) and consumed.
func TestRecoverStaleProxyRestoresRecordedState(t *testing.T) {
	path := withMarkerPath(t)

	backend := withFakeRecoveryBackend(t)

	// Simulate a crashed session: it enabled the proxy over a real
	// upstream proxy and never disabled it.
	crashed := recoveryRecord{
		Endpoint:    "127.0.0.1:10808",
		EnabledAtMS: 1758000000000,
		Previous: SystemProxySnapshot{
			Enabled: true,
			Server:  "proxy.corp.example:8080",
			Bypass:  []string{"localhost", "10.0.0.0/8"},
		},
	}

	blob, err := json.Marshal(crashed)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}

	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	found, err := RecoverStaleProxy()
	if err != nil || !found {
		t.Fatalf("RecoverStaleProxy = (%v, %v), want (true, nil)", found, err)
	}

	if _, ok := markerOnDisk(t, path); ok {
		t.Fatal("marker must be consumed after a successful restore")
	}

	if len(backend.restores) != 1 {
		t.Fatalf("restore called %d times, want exactly 1", len(backend.restores))
	}

	got := backend.restores[0]
	if !got.Enabled || got.Server != "proxy.corp.example:8080" || len(got.Bypass) != 2 {
		t.Fatalf("restored state = %+v, want the recorded previous state", got)
	}
}

// TestRecoverStaleProxyRetriesAfterFailedRestore proves the keep
// semantics: when the platform restore fails the marker stays, so the
// next boot retries (the machine still carries a stale proxy).
func TestRecoverStaleProxyRetriesAfterFailedRestore(t *testing.T) {
	path := withMarkerPath(t)

	withFakeRecoveryBackend(t).restoreErr = errors.New("wininet refused")

	crashed := recoveryRecord{
		Endpoint:    "127.0.0.1:10808",
		Previous:    SystemProxySnapshot{},
		EnabledAtMS: 1,
	}

	blob, err := json.Marshal(crashed)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}

	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	found, err := RecoverStaleProxy()
	if !found || err == nil {
		t.Fatalf("first recovery = (%v, %v), want (true, error)", found, err)
	}

	if _, ok := markerOnDisk(t, path); !ok {
		t.Fatal("marker was removed despite a failed restore; the next boot could not retry")
	}

	// The retry itself: a second call re-attempts (and fails again),
	// leaving the marker intact.
	found, err = RecoverStaleProxy()
	if !found || err == nil {
		t.Fatalf("second recovery = (%v, %v), want (true, error)", found, err)
	}

	if _, ok := markerOnDisk(t, path); !ok {
		t.Fatal("marker must survive repeated failed restores")
	}
}

// TestRecoverStaleProxyCorruptMarkerRemoved proves a malformed marker
// is treated as debris: removed and surfaced, never restored from.
func TestRecoverStaleProxyCorruptMarkerRemoved(t *testing.T) {
	path := withMarkerPath(t)

	if err := os.WriteFile(path, []byte("{\"previous\": not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt marker: %v", err)
	}

	found, err := RecoverStaleProxy()
	if !found {
		t.Fatal("RecoverStaleProxy must report the found marker")
	}

	if err == nil {
		t.Fatal("a corrupt marker must surface an error")
	}

	if _, ok := markerOnDisk(t, path); ok {
		t.Fatal("corrupt marker must be removed so it cannot fail every boot")
	}
}

// TestRecoverStaleProxyNoMarkerNoop proves the boot path with no
// residue: (false, nil), no platform call needed.
func TestRecoverStaleProxyNoMarkerNoop(t *testing.T) {
	withMarkerPath(t)

	found, err := RecoverStaleProxy()
	if found || err != nil {
		t.Fatalf("RecoverStaleProxy = (%v, %v), want (false, nil) with no marker", found, err)
	}
}

// TestRecoverStaleProxyNoPathNoop proves an unconfigured path (unit
// tests, embedded use) disables the mechanism entirely.
func TestRecoverStaleProxyNoPathNoop(t *testing.T) {
	prev := recoveryMarkerPath
	recoveryMarkerPath = ""
	t.Cleanup(func() { recoveryMarkerPath = prev })

	found, err := RecoverStaleProxy()
	if found || err != nil {
		t.Fatalf("RecoverStaleProxy = (%v, %v), want (false, nil) with no path", found, err)
	}
}

// TestRestoreRoutesThroughBackend proves the injected-backend contract
// used by the recovery path on Windows: Restore receives the recorded
// previous state verbatim and applies it as the new current state.
func TestRestoreRoutesThroughBackend(t *testing.T) {
	backend := &fakeProxyBackend{
		enabled: true,
		server:  "stale.freeiran.local:10808",
	}

	previous := SystemProxySnapshot{
		Enabled: true,
		Server:  "proxy.corp.example:8080",
		Bypass:  []string{"localhost"},
	}

	if err := backend.Restore(previous); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if len(backend.restores) != 1 {
		t.Fatalf("Restore called %d times, want exactly 1", len(backend.restores))
	}

	got := backend.restores[0]
	if !got.Enabled || got.Server != previous.Server || len(got.Bypass) != 1 {
		t.Fatalf("restored state = %+v, want %+v", got, previous)
	}

	if backend.server != previous.Server || !backend.enabled {
		t.Fatalf("backend current state = (%v, %q), want the restored state", backend.enabled, backend.server)
	}
}

// TestRecoveryMarkerDirectPrevious proves the common crash case:
// the user had NO proxy before FreeIran started (direct), so the
// marker records a direct snapshot and recovery resets to direct.
func TestRecoveryMarkerDirectPrevious(t *testing.T) {
	path := withMarkerPath(t)

	backend := &fakeProxyBackend{} // current: direct

	c := NewWithProxyBackend(backend)

	if err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	record, ok := markerOnDisk(t, path)
	if !ok {
		t.Fatal("marker missing after Enable")
	}

	if record.Previous.Enabled || record.Previous.Server != "" {
		t.Fatalf("marker previous = %+v, want the direct (empty) state", record.Previous)
	}
}
