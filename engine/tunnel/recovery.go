package tunnel

// recovery.go — crash-safe system-proxy restoration (v0.10.1).
//
// The system-proxy contract (§ "Windows system proxy") requires that
// application-owned proxy state is cleaned up on disconnect, shutdown,
// CRASH and reconnect. Disconnect and shutdown restore through
// Controller.Disable (the backend keeps the previous settings in
// memory). A crash skips that path entirely: the process dies, the
// in-memory previous-state is lost, and the machine is left with a
// system proxy pointing at a listener that no longer exists — the
// user's networking stays broken until it is fixed by hand.
//
// This file closes that gap with the same ownership-evidence pattern
// the process supervisor uses (system/process_manifest.go): while the
// system proxy is FreeIran-owned, a durable marker file records the
// PREVIOUS proxy state (the state to restore). At the next boot the
// application calls RecoverStaleProxy BEFORE any service starts; a
// present marker proves the previous session ended without a clean
// Disable, and the recorded previous state is restored exactly.
//
// Lifecycle rules:
//
//   - Controller.Enable (system proxy, success) writes the marker
//     holding the snapshot the backend saved as "previous".
//   - Controller.Disable (success) removes the marker: a clean
//     session leaves no residue.
//   - RecoverStaleProxy restores the recorded previous state and
//     removes the marker on success. A failed restore KEEPS the
//     marker so the next boot retries (the machine still carries a
//     foreign/stale proxy that must eventually be undone).
//   - A corrupt/unparseable marker is not trusted as restore data:
//     it is removed and the error surfaced (a malformed marker can
//     only be debris; restoring arbitrary values from it would be
//     worse than clearing it).
//   - The marker carries no credentials: WinINet proxy strings are
//     configuration, not secrets, and the redaction contract for
//     logs is unaffected.
//
// The marker path is set once by the application composition root
// (SetRecoveryMarkerPath, <workspace>/runtime/system-proxy.json) —
// the same pattern as system.SetProcessManifestPath. An empty path
// (tests, embedded use) disables persistence entirely.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// recoveryMarkerPath is the durable ownership marker location. Empty
// disables marker persistence (fresh test controllers never write).
var recoveryMarkerPath string

// recoveryBackend builds the backend a recovery replays through. The
// production value is the platform backend (WinINet on Windows); the
// indirection exists so the routing contract is provable on every
// platform CI runs on, exactly like NewWithProxyBackend does for the
// controller lifecycle.
var recoveryBackend = newSystemProxyBackend

// SetRecoveryMarkerPath configures where the system-proxy ownership
// marker is persisted. The application composition root calls it once
// at boot, before any tunnel controller exists.
func SetRecoveryMarkerPath(path string) {
	recoveryMarkerPath = path
}

// RecoveryMarkerPath returns the currently configured marker path
// (diagnostics/tests).
func RecoveryMarkerPath() string {
	return recoveryMarkerPath
}

// recoveryRecord is the marker payload: WHAT FreeIran set, WHEN, and
// the PREVIOUS state a recovery must restore. The previous state is
// captured by the backend on Enable and mirrored here so a crash
// cannot lose it.
type recoveryRecord struct {
	// Endpoint is the local proxy endpoint the session published
	// (diagnostics only — recovery never re-publishes it).
	Endpoint string `json:"endpoint"`

	// EnabledAtMS is the wall-clock millisecond timestamp of the
	// Enable that wrote the marker.
	EnabledAtMS int64 `json:"enabled_at_ms"`

	// Previous is the proxy state to restore on recovery.
	Previous SystemProxySnapshot `json:"previous"`
}

// markerLog surfaces marker I/O failures through the global logger.
// Marker persistence is best-effort by design (Enable/Disable already
// succeeded or will proceed), so these are warnings, never errors —
// but they are never silent either.
func markerLog(event string, err error) {
	logging.W(Subsystem, event, "system-proxy recovery marker: %s: %v", event, err)
}

func writeRecoveryMarker(previous SystemProxySnapshot, endpoint string) {
	if recoveryMarkerPath == "" {
		return
	}

	record := recoveryRecord{
		Endpoint:    endpoint,
		EnabledAtMS: time.Now().UTC().UnixMilli(),
		Previous:    previous,
	}

	blob, err := json.Marshal(record)
	if err != nil {
		// Cannot happen for this payload shape; still handled.
		markerLog("marker_marshal_failed", err)

		return
	}

	if err := os.MkdirAll(filepath.Dir(recoveryMarkerPath), 0o700); err != nil {
		markerLog("marker_mkdir_failed", err)

		return
	}

	tmp := recoveryMarkerPath + ".tmp"

	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		markerLog("marker_write_failed", err)

		return
	}

	if err := os.Rename(tmp, recoveryMarkerPath); err != nil {
		_ = os.Remove(tmp)

		markerLog("marker_write_failed", err)
	}
}

// clearRecoveryMarker removes the ownership marker after a clean
// Disable. Failure to remove keeps crash-safety conservative (the
// next boot restores the recorded previous state — which is exactly
// the state Disable just applied), so it is logged, never fatal.
func clearRecoveryMarker() {
	if recoveryMarkerPath == "" {
		return
	}

	if err := os.Remove(recoveryMarkerPath); err != nil && !os.IsNotExist(err) {
		markerLog("marker_remove_failed", err)
	}
}

// RecoverStaleProxy restores the system proxy recorded by a previous
// session that crashed while owning it. It is safe to call on every
// boot regardless of platform: with no marker configured, no marker
// present, or a backend that cannot restore, it reports honestly.
//
// The returned bool reports whether a stale marker was found (and
// therefore whether a recovery was attempted); the error carries the
// outcome of the attempt. A found-and-successfully-restored marker is
// (true, nil); no marker at all is (false, nil).
func RecoverStaleProxy() (bool, error) {
	if recoveryMarkerPath == "" {
		return false, nil
	}

	blob, err := os.ReadFile(recoveryMarkerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return true, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "recovery", "read system-proxy ownership marker")
	}

	var record recoveryRecord

	if err := json.Unmarshal(blob, &record); err != nil {
		// Debris, not evidence: drop it so it cannot fail every
		// future boot, and surface the corruption.
		_ = os.Remove(recoveryMarkerPath)

		return true, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "recovery", "parse system-proxy ownership marker (removed)")
	}

	if err := recoveryBackend().Restore(record.Previous); err != nil {
		// The machine still carries the stale proxy: KEEP the
		// marker so the next boot retries the restoration.
		return true, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "recovery", "restore previous system-proxy state")
	}

	clearRecoveryMarker()

	return true, nil
}
