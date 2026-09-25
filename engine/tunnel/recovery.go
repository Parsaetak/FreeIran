package tunnel

// recovery.go — crash-safe, TRANSACTIONAL system-proxy restoration
// (redesigned in v0.10.2).
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
// v0.10.2 transactional ownership invariant:
//
//      FreeIran changes the system proxy
//      ⇒ durable ownership evidence already exists
//
// The ownership sequence (Controller.Enable, ModeSystemProxy):
//
//      capture previous platform state   (backend.Current)
//      → durably persist validated recovery record   (phase=pending)
//      → activate FreeIran proxy                      (backend.Enable)
//      → verify the resulting platform state          (backend.Current)
//      → mark ownership ACTIVE                        (phase=active)
//
// A crash between record-write and activation restores the recorded
// previous state at boot — which is a no-op when the activation never
// ran, and exact otherwise. An activation or verification failure
// rolls back through backend.Restore and consumes the marker; a
// rollback restore that itself fails KEEPS the marker (the record is
// exactly the state being rolled back to, so the next boot retries
// it) and the error says so.
//
// Boot recovery (RecoverStaleProxy) is equally strict:
//
//      read marker
//      → validate schema/content
//      → restore previous state            (backend.Restore)
//      → verify the ACTUAL platform state matches the record
//      → only then remove the marker
//
// If marker cleanup fails, an explicit recovery-residual error
// (ErrOwnershipResidual) is returned and the marker is preserved.
// Never silently claim recovery succeeded while durable ownership
// residue remains.
//
// Marker schema (on-disk, cross-version contract):
//
//      { "schema_version": 2, "phase": "active"|"pending",
//        "endpoint": "...", "enabled_at_ms": ...,
//        "previous": { ... SystemProxySnapshot ... } }
//
// v0.10.1 workspaces carry version-1 markers: no schema_version, no
// phase. Those parse unchanged (missing version = 1, missing phase =
// pending) and recover with the documented v1 fidelity limit (the PAC
// URL / autodetect bits were not captured in v0.10.1).
//
// Lifecycle rules:
//
//   - Enable (system proxy) writes the marker BEFORE activation.
//   - Disable (success) removes the marker; a removal failure is the
//     explicit ErrOwnershipResidual error.
//   - RecoverStaleProxy restores, VERIFIES, then removes; a failed
//     restore or failed verification KEEPS the marker so the next
//     boot retries.
//   - A corrupt/unparseable or invalid marker is not trusted as
//     restore data: it is removed and the error surfaced (restoring
//     arbitrary values from debris would be worse than clearing it).
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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// markerSchemaVersion is the current on-disk marker schema. Version 1
// is the v0.10.1 shape (no schema_version / phase fields); version 2
// adds both plus the snapshot fidelity fields (AutoConfigURL,
// AutoDetect, Flags).
const markerSchemaVersion = 2

// ownershipPhase labels where in the ownership transaction the
// process was when the marker was last written. Boot recovery treats
// both phases identically (restore the recorded previous state); the
// phase exists so diagnostics can distinguish "crashed before
// activation" from "crashed while owning the proxy".
type ownershipPhase string

const (
	phasePending ownershipPhase = "pending"
	phaseActive  ownershipPhase = "active"
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

// recoveryRecord is the marker payload: WHAT FreeIran set, WHEN, the
// PREVIOUS state a recovery must restore, and the ownership PHASE at
// the time of the last durable write.
type recoveryRecord struct {
	// SchemaVersion pins the on-disk contract. Absent (=0) means the
	// v0.10.1 legacy shape.
	SchemaVersion int `json:"schema_version,omitempty"`

	// Phase is the ownership transaction phase when the record was
	// last written ("pending" or "active"). Absent = pending.
	Phase ownershipPhase `json:"phase,omitempty"`

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
func markerLog(event string, err error) {
	logging.W(Subsystem, event, "system-proxy recovery marker: %s: %v", event, err)
}

// writeRecoveryMarker durably persists the validated recovery record.
//
// v0.10.2: this is a TRANSACTION step, not a best-effort log line —
// the returned error aborts Controller.Enable before the proxy is
// touched. Durability: write-to-temp → fsync(file) → rename over the
// target (the Windows-durable write pattern; a rename either happens
// or does not, so the marker is never half-written).
func writeRecoveryMarker(previous SystemProxySnapshot, endpoint string) error {
	if recoveryMarkerPath == "" {
		return nil
	}

	record := recoveryRecord{
		SchemaVersion: markerSchemaVersion,
		Phase:         phasePending,
		Endpoint:      endpoint,
		EnabledAtMS:   time.Now().UTC().UnixMilli(),
		Previous:      previous,
	}

	if err := validateRecoveryRecord(record); err != nil {
		return fmt.Errorf("recovery record invalid before write: %w", err)
	}

	return writeRecoveryRecord(record)
}

// markOwnershipActive rewrites the marker with phase=active after the
// activation was verified. It is read-modify-write on the same record
// so a concurrent crash recovery never sees a torn file.
func markOwnershipActive(endpoint string) error {
	if recoveryMarkerPath == "" {
		return nil
	}

	record, err := readRecoveryRecord()
	if err != nil {
		return err
	}

	record.Phase = phaseActive
	if endpoint != "" {
		record.Endpoint = endpoint
	}

	return writeRecoveryRecord(record)
}

// writeRecoveryRecord atomically writes the record file.
func writeRecoveryRecord(record recoveryRecord) error {
	blob, err := json.Marshal(record)
	if err != nil {
		// Cannot happen for this payload shape; still handled.
		return fmt.Errorf("marshal recovery record: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(recoveryMarkerPath), 0o700); err != nil {
		return fmt.Errorf("create marker directory: %w", err)
	}

	tmp := recoveryMarkerPath + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create marker temp file: %w", err)
	}

	if _, err := f.Write(blob); err != nil {
		f.Close()
		os.Remove(tmp)

		return fmt.Errorf("write marker: %w", err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)

		return fmt.Errorf("sync marker: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(tmp)

		return fmt.Errorf("close marker: %w", err)
	}

	if err := os.Rename(tmp, recoveryMarkerPath); err != nil {
		os.Remove(tmp)

		return fmt.Errorf("publish marker: %w", err)
	}

	return nil
}

// readRecoveryRecord reads and parses the marker. os.ErrNotExist is
// returned unwrapped so callers can distinguish "no marker". Parse
// failures are marked with the parseFailure wrapper so callers can
// tell debris apart from I/O trouble.
type parseFailure struct{ err error }

func (p parseFailure) Error() string { return p.err.Error() }
func (p parseFailure) Unwrap() error { return p.err }

func isParseFailure(err error) bool {
	var pf parseFailure
	return errors.As(err, &pf)
}

func readRecoveryRecord() (recoveryRecord, error) {
	var record recoveryRecord

	blob, err := os.ReadFile(recoveryMarkerPath)
	if err != nil {
		return record, err
	}

	if err := json.Unmarshal(blob, &record); err != nil {
		return record, parseFailure{fmt.Errorf("parse system-proxy ownership marker: %w", err)}
	}

	return record, nil
}

// validateRecoveryRecord rejects records a recovery must not trust:
// unknown schema versions, absurd string payloads, control characters
// that would poison logs or the registry-bound WinINet strings. A
// record failing validation is debris, not evidence.
func validateRecoveryRecord(record recoveryRecord) error {
	switch record.SchemaVersion {
	case 0, 1, 2:
		// 0 = legacy v0.10.1 shape (no version field).
	default:
		return fmt.Errorf("unsupported marker schema version %d", record.SchemaVersion)
	}

	if record.Phase != "" && record.Phase != phasePending && record.Phase != phaseActive {
		return fmt.Errorf("unknown ownership phase %q", record.Phase)
	}

	if err := validateMarkerString("endpoint", record.Endpoint, 256); err != nil {
		return err
	}

	if err := validateMarkerString("proxy server", record.Previous.Server, 2048); err != nil {
		return err
	}

	if err := validateMarkerString("autoconfig URL", record.Previous.AutoConfigURL, 2048); err != nil {
		return err
	}

	for _, entry := range record.Previous.Bypass {
		if err := validateMarkerString("bypass entry", entry, 1024); err != nil {
			return err
		}
	}

	return nil
}

// validateMarkerString enforces length and printable-ASCII/Unicode
// sanity: no control characters, no NULs (WinINet strings are
// NUL-terminated — an embedded NUL silently truncates them).
func validateMarkerString(what, value string, maxLen int) error {
	if len(value) > maxLen {
		return fmt.Errorf("%s exceeds %d bytes", what, maxLen)
	}

	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a control character (U+%04X)", what, r)
		}
	}

	return nil
}

// clearRecoveryMarker removes the ownership marker after a clean
// Disable or a verified recovery. v0.10.2: the error is REAL — a
// marker that survives a clean outcome is ownership residue and is
// reported as such (ErrOwnershipResidual at the call sites).
func clearRecoveryMarker() error {
	if recoveryMarkerPath == "" {
		return nil
	}

	if err := os.Remove(recoveryMarkerPath); err != nil && !os.IsNotExist(err) {
		markerLog("marker_remove_failed", err)

		return fmt.Errorf("remove ownership marker: %w", err)
	}

	return nil
}

// OwnershipStatus is the redacted ownership view for the UI: whether
// a marker exists, its phase, endpoint, age and the recorded previous
// state — the "FreeIran proxy ownership / saved previous state /
// recovery status" surface (v0.10.2 § Windows system-proxy UX).
type OwnershipStatus struct {
	Present       bool                `json:"present"`
	Phase         string              `json:"phase,omitempty"`
	SchemaVersion int                 `json:"schema_version,omitempty"`
	Endpoint      string              `json:"endpoint,omitempty"`
	EnabledAtMS   int64               `json:"enabled_at_ms,omitempty"`
	Previous      SystemProxySnapshot `json:"previous,omitempty"`
}

// CurrentOwnershipStatus reports the durable ownership marker state
// without touching the platform proxy settings.
func CurrentOwnershipStatus() OwnershipStatus {
	if recoveryMarkerPath == "" {
		return OwnershipStatus{}
	}

	record, err := readRecoveryRecord()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OwnershipStatus{}
		}

		return OwnershipStatus{Present: true, Phase: "unreadable"}
	}

	return OwnershipStatus{
		Present:       true,
		Phase:         string(record.Phase),
		SchemaVersion: record.SchemaVersion,
		Endpoint:      record.Endpoint,
		EnabledAtMS:   record.EnabledAtMS,
		Previous:      record.Previous,
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
//
// v0.10.2 verification gate: after Restore, the ACTUAL platform state
// is read back and compared (normalized) against the record. Only a
// verified match consumes the marker; anything else — restore
// failure, verification failure, verification mismatch, marker
// cleanup failure — keeps the marker and returns an explicit error.
func RecoverStaleProxy() (bool, error) {
	if recoveryMarkerPath == "" {
		return false, nil
	}

	record, err := readRecoveryRecord()
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		// Distinguish an I/O failure (marker kept — the evidence is
		// intact, the read failed) from an unparseable marker (debris
		// — removed so it cannot fail every future boot).
		if isParseFailure(err) {
			_ = clearRecoveryMarker()

			return true, firerrors.Wrap(fmt.Errorf("corrupt system-proxy ownership marker (removed): %w", err),
				firerrors.KindCorruptData,
				Subsystem, "recovery", "parse system-proxy ownership marker")
		}

		return true, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "recovery", "read system-proxy ownership marker")
	}

	// Validation happens BEFORE any platform mutation. A record that
	// cannot be trusted is debris: drop it so it cannot fail every
	// future boot, and surface the corruption.
	if err := validateRecoveryRecord(record); err != nil {
		_ = clearRecoveryMarker()

		return true, firerrors.Wrap(fmt.Errorf("invalid system-proxy ownership marker (removed): %w", err),
			firerrors.KindCorruptData,
			Subsystem, "recovery", "validate system-proxy ownership marker")
	}

	backend := recoveryBackend()

	if err := backend.Restore(record.Previous); err != nil {
		// The machine still carries the stale proxy: KEEP the
		// marker so the next boot retries the restoration.
		return true, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "recovery", "restore previous system-proxy state")
	}

	// Verification gate: the ACTUAL platform state must now match the
	// record we were asked to restore. In-memory success is not
	// evidence.
	observed, err := backend.Current()
	if err != nil {
		return true, firerrors.Wrap(fmt.Errorf("verify restored state: %w", err),
			firerrors.KindEnvironment,
			Subsystem, "recovery", "read back system-proxy state after restore")
	}

	if !proxyStatesEqual(observed, record.Previous) {
		return true, firerrors.Wrap(fmt.Errorf(
			"restored state verification failed: platform reports %q mode, record expects the recorded previous state",
			proxyModeOf(observed)),
			firerrors.KindEnvironment,
			Subsystem, "recovery", "verify system-proxy restoration")
	}

	// Verified: consume the marker. A cleanup failure is an explicit
	// recovery-residual error — the restore itself succeeded, but the
	// durable ownership evidence remains and the user must know.
	if err := clearRecoveryMarker(); err != nil {
		return true, firerrors.Wrap(fmt.Errorf("%w: %v", ErrOwnershipResidual, err),
			firerrors.KindEnvironment,
			Subsystem, "recovery", "consume verified system-proxy ownership marker")
	}

	return true, nil
}
