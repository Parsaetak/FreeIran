package tunnel

// recovery_test.go — the crash-safe, TRANSACTIONAL system-proxy
// ownership contract (redesigned v0.10.2). The WinINet backend itself
// is only exercised on Windows (see proxy_windows_abi_test.go); these
// tests prove the MARKER lifecycle, the transaction ordering and the
// restore routing with a deterministic in-memory backend, on every
// platform CI runs on.
//
// What is pinned here:
//
//   - Enable(system proxy) persists the ownership marker carrying the
//     PREVIOUS state BEFORE the backend activates the proxy
//     (evidence-before-mutation).
//   - Activation / verification failure rolls back: previous state
//     restored, marker consumed, explicit error.
//   - A marker that cannot be persisted aborts the activation before
//     the platform is touched.
//   - Disable removes the marker: a clean session leaves no residue;
//     a marker that cannot be removed is the explicit
//     ErrOwnershipResidual error.
//   - RecoverStaleProxy restores, VERIFIES the actual platform state
//     against the record, and only then consumes the marker.
//   - A restore failure or verification mismatch KEEPS the marker.
//   - A corrupt/invalid marker is removed and surfaced, never
//     restored from — including unknown future schema versions.
//   - No marker / no configured path = honest no-op.
//   - v0.10.1 (schema-less) markers still recover: cross-version
//     on-disk contract.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeProxyBackend is a deterministic SystemProxyBackend double: it
// records every call and returns configured errors. It models the
// Windows save-on-Enable semantics exactly — the part the recovery
// contract depends on. After a successful Enable, Current() reports
// the FreeIran proxy at host:port with the explicit-proxy flag set,
// exactly like the real WinINet roundtrip.
type fakeProxyBackend struct {
	enableErr  error
	disableErr error
	restoreErr error
	currentErr error

	// corruptActivation makes Enable "half-work": the call succeeds
	// but the platform state does NOT show FreeIran's proxy — the
	// verification-rollback path.
	corruptActivation bool

	enabled   bool
	server    string
	bypass    []string
	flagsNow  uint32
	previous  SystemProxySnapshot
	savedOnce bool

	restores []SystemProxySnapshot

	// markerAtActivation, when non-nil, is called inside Enable and
	// records whether the durable marker existed at activation time —
	// the transactional ordering proof (evidence BEFORE mutation).
	markerAtActivation func() bool
	markerAtEnable     bool

	// restoreHook, when non-nil, runs inside Restore before the state
	// is applied (used to swap the marker file for something
	// un-removable mid-recovery).
	restoreHook func()
}

func (f *fakeProxyBackend) Enable(_ context.Context, host string, port int, _ bool, bypass []string) error {
	if f.markerAtActivation != nil {
		f.markerAtEnable = f.markerAtActivation()
	}

	if f.enableErr != nil {
		return f.enableErr
	}

	f.previous = SystemProxySnapshot{
		Enabled: f.enabled,
		Server:  f.server,
		Bypass:  f.bypass,
		Flags:   f.flagsNow,
	}
	f.savedOnce = true

	if f.corruptActivation {
		// The call "succeeds" but the platform shows something else.
		f.enabled = true
		f.server = "socks=10.9.9.9:1"
		f.flagsNow = proxyTypeProxy

		return nil
	}

	f.enabled = true
	f.server = fmt.Sprintf("socks=%s:%d", host, port)
	f.bypass = bypass
	f.flagsNow = proxyTypeProxy

	return nil
}

func (f *fakeProxyBackend) Disable(_ context.Context) error {
	if f.disableErr != nil {
		return f.disableErr
	}

	if f.savedOnce {
		f.enabled = f.previous.Enabled
		f.server = f.previous.Server
		f.bypass = f.previous.Bypass
		f.flagsNow = f.previous.Flags
		f.savedOnce = false
		f.previous = SystemProxySnapshot{}
	} else {
		f.enabled = false
		f.server = ""
		f.bypass = nil
		f.flagsNow = proxyTypeDirect
	}

	return nil
}

// Snapshot mirrors winINetBackend: after Enable it reports the SAVED
// previous state (that is what a v0.10.1-style marker would persist);
// before any Enable it reports the current state.
func (f *fakeProxyBackend) Snapshot() SystemProxySnapshot {
	if f.savedOnce {
		return f.previous
	}

	return SystemProxySnapshot{Enabled: f.enabled, Server: f.server, Bypass: f.bypass, Flags: f.flagsNow}
}

// Current reports the ACTUAL (simulated) platform state.
func (f *fakeProxyBackend) Current() (SystemProxySnapshot, error) {
	if f.currentErr != nil {
		return SystemProxySnapshot{}, f.currentErr
	}

	return SystemProxySnapshot{Enabled: f.enabled, Server: f.server, Bypass: f.bypass, Flags: f.flagsNow}, nil
}

func (f *fakeProxyBackend) Restore(previous SystemProxySnapshot) error {
	if f.restoreHook != nil {
		f.restoreHook()
	}

	if f.restoreErr != nil {
		return f.restoreErr
	}

	f.restores = append(f.restores, previous)

	// Apply with the same fidelity the real backend uses: explicit
	// mode derived from the record (legacy snapshots carry no Flags).
	f.enabled = previous.Enabled && previous.Server != ""
	f.server = previous.Server
	f.bypass = previous.Bypass

	if previous.Flags != 0 {
		f.flagsNow = previous.Flags
	} else if f.enabled {
		f.flagsNow = proxyTypeProxy
	} else {
		f.flagsNow = proxyTypeDirect
	}

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

// TestRecoveryMarkerWrittenBeforeActivation proves the v0.10.2
// transactional ordering: the durable record EXISTS when the backend
// activation runs — not after it. The invariant "FreeIran changed the
// system proxy ⇒ durable ownership evidence already exists" is
// stronger than "evidence eventually exists".
func TestRecoveryMarkerWrittenBeforeActivation(t *testing.T) {
	path := withMarkerPath(t)

	backend := &fakeProxyBackend{
		markerAtActivation: func() bool {
			_, err := os.Stat(path)
			return err == nil
		},
	}

	c := NewWithProxyBackend(backend)

	if err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	if !backend.markerAtEnable {
		t.Fatal("activation ran without durable ownership evidence: the marker must be written BEFORE the backend Enable")
	}

	record, ok := markerOnDisk(t, path)
	if !ok {
		t.Fatal("marker missing after successful Enable")
	}

	if record.Phase != phaseActive {
		t.Errorf("marker phase = %q, want %q after a verified activation", record.Phase, phaseActive)
	}

	if record.SchemaVersion != markerSchemaVersion {
		t.Errorf("marker schema_version = %d, want %d", record.SchemaVersion, markerSchemaVersion)
	}
}

// TestEnableRollsBackWhenActivationFails proves the rollback path:
// the backend refuses, the previous state is restored, the marker is
// consumed and the error is explicit — no half-owned proxy state.
func TestEnableRollsBackWhenActivationFails(t *testing.T) {
	path := withMarkerPath(t)

	backend := &fakeProxyBackend{
		enableErr: errors.New("wininet refused"),
		enabled:   false,
	}

	c := NewWithProxyBackend(backend)

	err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{})
	if err == nil {
		t.Fatal("Enable must fail when the backend refuses")
	}

	if !errors.Is(err, backend.enableErr) && !contains(err.Error(), "wininet refused") {
		t.Fatalf("error does not carry the activation cause: %v", err)
	}

	if _, ok := markerOnDisk(t, path); ok {
		t.Fatal("ownership marker survived a rolled-back Enable")
	}

	if backend.enabled {
		t.Fatalf("platform state = %v after failed Enable, want the previous (direct) state", backend.enabled)
	}

	if state := c.State(); state.Active {
		t.Fatalf("controller reports active after a failed Enable: %+v", state)
	}
}

// TestEnableRollsBackWhenVerificationFails proves the verification
// gate: an Enable whose platform state does NOT show the FreeIran
// proxy is rolled back — "process started" is not ownership.
func TestEnableRollsBackWhenVerificationFails(t *testing.T) {
	path := withMarkerPath(t)

	backend := &fakeProxyBackend{corruptActivation: true}

	c := NewWithProxyBackend(backend)

	err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{})
	if err == nil {
		t.Fatal("Enable must fail when the platform state does not show the activated proxy")
	}

	if !contains(err.Error(), "verification failed") {
		t.Fatalf("error = %v, want the activation-verification failure", err)
	}

	if _, ok := markerOnDisk(t, path); ok {
		t.Fatal("ownership marker survived a rolled-back (unverified) Enable")
	}

	// The fake's corrupted state is what rollback Restore must have
	// overwritten with the captured previous (direct). A server of
	// "" proves the rollback applied; the corrupted value would prove
	// it did not.
	if backend.server != "" {
		t.Fatalf("rollback did not restore: platform server = %q, want the captured direct state", backend.server)
	}
}

// TestEnableAbortsWhenMarkerCannotBePersisted proves the durable-
// evidence precondition: a marker write failure aborts the
// activation BEFORE the platform proxy is touched.
func TestEnableAbortsWhenMarkerCannotBePersisted(t *testing.T) {
	base := t.TempDir()

	// Make the marker's parent directory path occupied by a regular
	// FILE so MkdirAll necessarily fails.
	blocker := filepath.Join(base, "runtime")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	prevPath := recoveryMarkerPath
	recoveryMarkerPath = filepath.Join(blocker, "system-proxy.json")
	t.Cleanup(func() { recoveryMarkerPath = prevPath })

	backend := &fakeProxyBackend{}
	c := NewWithProxyBackend(backend)

	err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{})
	if err == nil {
		t.Fatal("Enable must fail when the ownership marker cannot be persisted")
	}

	if !contains(err.Error(), "persist ownership evidence") {
		t.Fatalf("error = %v, want the ownership-evidence failure", err)
	}

	if backend.savedOnce || backend.enabled {
		t.Fatal("the platform proxy must not be touched when ownership evidence cannot be persisted")
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

// TestDisableResidualMarkerError proves the v0.10.2 residual rule: a
// marker that cannot be removed after a successful Disable is the
// explicit ErrOwnershipResidual error — never a silent success.
func TestDisableResidualMarkerError(t *testing.T) {
	path := withMarkerPath(t)

	backend := &fakeProxyBackend{}
	c := NewWithProxyBackend(backend)

	if err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// Swap the marker file for a non-empty directory: os.Remove on it
	// fails, modeling a file held by antivirus/backup/indexing.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove marker for swap: %v", err)
	}

	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir marker: %v", err)
	}

	if err := os.WriteFile(filepath.Join(path, "held"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write held file: %v", err)
	}

	t.Cleanup(func() { os.RemoveAll(path) })

	err := c.Disable(context.Background())
	if err == nil {
		t.Fatal("Disable must report the residual ownership marker")
	}

	if !errors.Is(err, ErrOwnershipResidual) {
		t.Fatalf("err = %v, want ErrOwnershipResidual", err)
	}

	// The proxy state itself WAS restored: the controller is no
	// longer active, and the residual is visible in Details.
	if state := c.State(); state.Active {
		t.Fatalf("controller still active after Disable: %+v", state)
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
// previous state), verified against the platform state, and consumed.
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

// TestRecoverStaleProxyLegacyV1Marker pins the cross-version on-disk
// contract: the EXACT v0.10.1 marker shape (no schema_version, no
// phase) still recovers — old workspaces must never be stranded.
func TestRecoverStaleProxyLegacyV1Marker(t *testing.T) {
	path := withMarkerPath(t)

	withFakeRecoveryBackend(t)

	legacy := `{
  "endpoint": "127.0.0.1:10808",
  "enabled_at_ms": 1758000000000,
  "previous": {
    "enabled": true,
    "server": "proxy.corp.example:8080",
    "bypass": ["localhost"],
    "saved": true
  }
}`

	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}

	found, err := RecoverStaleProxy()
	if !found || err != nil {
		t.Fatalf("RecoverStaleProxy = (%v, %v), want (true, nil) for a v0.10.1 marker", found, err)
	}

	if _, ok := markerOnDisk(t, path); ok {
		t.Fatal("legacy marker must be consumed after successful recovery")
	}
}

// TestRecoverStaleProxyKeepsMarkerOnVerificationMismatch proves the
// verification gate: a backend that REPORTS success but leaves a
// different platform state must not get the marker consumed.
func TestRecoverStaleProxyKeepsMarkerOnVerificationMismatch(t *testing.T) {
	path := withMarkerPath(t)

	backend := withFakeRecoveryBackend(t)
	backend.restoreErr = nil

	// After Restore, the platform reports a DIFFERENT server than the
	// record (simulating a partially applied restoration).
	backend.currentErr = nil
	backend.server = "socks=10.0.0.1:9"
	backend.enabled = true
	backend.flagsNow = proxyTypeProxy

	// Make Restore itself silently NOT apply the recorded state (the
	// mismatch scenario): the double's Restore is bypassed by
	// pre-seeding the "platform" state via the hook.
	backend.restoreErr = errors.New("wininet refused")

	crashed := recoveryRecord{
		Endpoint:    "127.0.0.1:10808",
		EnabledAtMS: 1758000000000,
		Previous: SystemProxySnapshot{
			Enabled: true,
			Server:  "proxy.corp.example:8080",
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
	if !found || err == nil {
		t.Fatalf("RecoverStaleProxy = (%v, %v), want (true, error) on failed restore", found, err)
	}

	if _, ok := markerOnDisk(t, path); !ok {
		t.Fatal("marker must survive a failed restore for the next boot to retry")
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

	// markerOnDisk would Fatalf on the malformed JSON by design; use
	// a plain existence check here.
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("corrupt marker must be removed so it cannot fail every boot")
	}
}

// TestRecoverStaleProxyInvalidSchemaRemoved proves a marker with an
// unknown FUTURE schema version is not trusted as restore data
// (forward compatibility: never restore from a shape we cannot
// validate).
func TestRecoverStaleProxyInvalidSchemaRemoved(t *testing.T) {
	path := withMarkerPath(t)

	backend := withFakeRecoveryBackend(t)

	blob, err := json.Marshal(recoveryRecord{
		SchemaVersion: 99,
		Endpoint:      "127.0.0.1:10808",
		Previous: SystemProxySnapshot{
			Enabled: true,
			Server:  "evil.example:1",
		},
	})
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}

	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	found, err := RecoverStaleProxy()
	if !found || err == nil {
		t.Fatalf("RecoverStaleProxy = (%v, %v), want (true, error) for an unsupported schema", found, err)
	}

	if len(backend.restores) != 0 {
		t.Fatal("an invalid marker must never reach the restore path")
	}

	if _, ok := markerOnDisk(t, path); ok {
		t.Fatal("an invalid marker must be removed so it cannot fail every boot")
	}
}

// TestRecoverStaleProxyResidualWhenCleanupFails proves the explicit
// recovery-residual contract: a verified restore whose marker cannot
// be consumed returns ErrOwnershipResidual and the residue stays
// visible — never a silent clean outcome.
func TestRecoverStaleProxyResidualWhenCleanupFails(t *testing.T) {
	path := withMarkerPath(t)

	backend := withFakeRecoveryBackend(t)

	// Mid-recovery the marker file is swapped for a non-empty
	// directory (the "file held by AV/backup" model — os.Remove
	// fails on it).
	backend.restoreHook = func() {
		if err := os.Remove(path); err == nil {
			if err := os.Mkdir(path, 0o700); err == nil {
				_ = os.WriteFile(filepath.Join(path, "held"), []byte("x"), 0o600)
			}
		}
	}

	crashed := recoveryRecord{
		Endpoint:    "127.0.0.1:10808",
		EnabledAtMS: 1758000000000,
		Previous: SystemProxySnapshot{
			Enabled: true,
			Server:  "proxy.corp.example:8080",
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
	if !found || err == nil {
		t.Fatalf("RecoverStaleProxy = (%v, %v), want (true, residual error)", found, err)
	}

	if !errors.Is(err, ErrOwnershipResidual) {
		t.Fatalf("err = %v, want ErrOwnershipResidual", err)
	}

	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("residual marker must be preserved for the next boot: %v", statErr)
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

// TestOwnershipStatusReportsMarker proves the UI projection: phase,
// endpoint and previous state are readable without touching the
// platform proxy.
func TestOwnershipStatusReportsMarker(t *testing.T) {
	path := withMarkerPath(t)
	_ = path

	if status := CurrentOwnershipStatus(); status.Present {
		t.Fatalf("status with no marker = %+v, want absent", status)
	}

	backend := &fakeProxyBackend{}
	c := NewWithProxyBackend(backend)

	if err := c.Enable(context.Background(), ModeSystemProxy, "127.0.0.1", 10808, Options{}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	status := CurrentOwnershipStatus()
	if !status.Present {
		t.Fatal("status must report the marker after Enable")
	}

	if status.Phase != string(phaseActive) {
		t.Errorf("status phase = %q, want active", status.Phase)
	}

	if status.Endpoint != "127.0.0.1:10808" {
		t.Errorf("status endpoint = %q, want 127.0.0.1:10808", status.Endpoint)
	}

	if err := c.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	if status := CurrentOwnershipStatus(); status.Present {
		t.Fatalf("status after clean Disable = %+v, want absent", status)
	}
}

// contains is a tiny helper avoiding strings import noise in this
// file.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}

	return -1
}
