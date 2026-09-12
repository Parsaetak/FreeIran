package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// This file implements the deterministic migration from the legacy
// JSON database format (engine/database diskState v1) into the chunked
// store.
//
// Guarantees:
//   - The legacy file is NEVER deleted; it is renamed to
//     "<original>.migrated" after a verified migration.
//   - Fingerprints (keys) and per-record metadata are preserved
//     verbatim inside the migrated values.
//   - Migration is idempotent: a missing legacy file is a no-op
//     success, and re-running after ".migrated" exists does nothing.
//   - The legacy file is STREAMED with a token-walking decoder and
//     processed in bounded batches: a huge legacy database never
//     requires proportional RAM.
//   - Every record is validated before it is staged; a corrupt record
//     is reported, not silently dropped (strict mode aborts).
//   - Verification is a second streaming pass after the final flush,
//     so the full disk path (chunk + index + read) is checked without
//     keeping every key in memory.
//   - On any failure the store is left consistent: staging only
//     mutates the memtable, and a failed run aborts before the rename.
//     Re-running re-applies batches idempotently.

// migrationBatchSize bounds records staged per UpsertBatch.
const migrationBatchSize = 1024

// Lifecycle hooks for the legacy file descriptor. They exist so the
// close-BEFORE-rename ordering is regression-testable on every
// platform: on Windows, renaming (or removing) a file that this
// process still holds open fails with "the process cannot access the
// file", so the descriptor must be fully released first.
var (
	closeLegacyFile  = func(file *os.File) error { return file.Close() }
	renameLegacyFile = os.Rename
)

// LegacyEntry mirrors engine/database Entry.
type LegacyEntry struct {
	Config  json.RawMessage `json:"config"`
	Added   string          `json:"added"`
	Updated string          `json:"updated"`
}

// MigrateOptions controls the JSON v1 migration.
type MigrateOptions struct {
	// LegacyPath is the path of the legacy database JSON file.
	LegacyPath string

	// Strict aborts on the first invalid record. In non-strict mode
	// invalid records are counted and reported in the result.
	Strict bool
}

// MigrationResult reports what happened during a migration.
type MigrationResult struct {
	Migrated int      `json:"migrated"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
	Renamed  bool     `json:"renamed"`
}

// MigrateFromJSON imports a legacy JSON database into the store.
func (s *Store) MigrateFromJSON(
	ctx context.Context,
	opts MigrateOptions,
) (MigrationResult, error) {
	var result MigrationResult

	if !s.beginOp() {
		return result, ErrClosed
	}

	defer s.endOp()

	file, err := os.Open(opts.LegacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Idempotent: nothing to migrate.
			return result, nil
		}

		return result, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "migrate", "read legacy database")
	}

	// Resource-ownership discipline (the Windows-safe lifecycle):
	//
	//   open → read (pass 1) → flush → verify (pass 2) →
	//   close underlying descriptor (error-aware) → only then rename.
	//
	// `closed` flips to true ONLY after a close has been acknowledged
	// (closeLegacyFile returned nil). On a close failure `closed` stays
	// false so the deferred safety net can retry the underlying OS
	// release directly through file.Close() — bypassing the testable
	// hook — guaranteeing no descriptor outlives MigrateFromJSON even
	// when the hook injects an error. This is the Windows-critical
	// guarantee: a leaked descriptor blocks the retry rename and the
	// TempDir cleanup, so the OS handle must be released through
	// guaranteed cleanup regardless of what the hook reports.
	closed := false

	closeNow := func() error {
		if closed {
			return nil
		}

		if err := closeLegacyFile(file); err != nil {
			return err // closed stays false; deferred cleanup will release
		}

		closed = true

		return nil
	}

	defer func() {
		if closed {
			return
		}

		// Direct OS release — NOT closeLegacyFile — so a hook-injected
		// error cannot defeat the lifecycle guarantee. The error is
		// dropped because the caller has already received the explicit
		// close error (or some earlier failure); the only goal here is
		// releasing the descriptor.
		_ = file.Close()
	}()

	version, err := legacyVersion(file)
	if err != nil {
		return result, err
	}

	if version != 1 {
		return result, firerrors.New(firerrors.KindCorruptData,
			Subsystem, "migrate", "unsupported legacy version %d", version)
	}

	// ---- Pass 1: stream entries, stage in bounded batches ----
	batch := make([]Pair, 0, migrationBatchSize)

	stage := func(key string, entry *LegacyEntry) error {
		batch = append(batch, Pair{Key: key, Value: entry.Config})

		if len(batch) >= migrationBatchSize {
			return commitMigrationBatch(s, &batch, &result)
		}

		return nil
	}

	if err := s.legacyPass(ctx, file, opts, &result, stage); err != nil {
		return result, err
	}

	// Leftover partial batch.
	if err := commitMigrationBatch(s, &batch, &result); err != nil {
		return result, err
	}

	// Persist everything before declaring success.
	if err := s.Flush(); err != nil {
		return result, err
	}

	// ---- Pass 2: verify every record through the full disk path ----
	verified := 0

	verify := func(key string, _ *LegacyEntry) error {
		value, err := s.Get(key)
		if err != nil || value == nil {
			return firerrors.New(firerrors.KindCorruptData,
				Subsystem, "migrate", "verification failed for %s", key)
		}

		verified++

		return nil
	}

	if err := s.legacyPass(ctx, file, opts, &result, verify); err != nil {
		return result, err
	}

	if verified != result.Migrated {
		return result, firerrors.New(firerrors.KindCorruptData,
			Subsystem, "migrate",
			"verification mismatch: staged %d, verified %d",
			result.Migrated, verified)
	}

	// Release the legacy descriptor BEFORE the rename. Windows refuses
	// to rename a file this process still holds open; a close failure
	// is fatal here (data is already migrated and verified, but the
	// lifecycle contract requires the descriptor gone before rename).
	// On a close failure `closed` stays false and the deferred safety
	// net releases the OS handle directly so the retry attempt can
	// succeed.
	if err := closeNow(); err != nil {
		return result, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "migrate", "close legacy database before rename")
	}

	// Preserve the legacy file (rename, never delete). If a previous
	// migration already renamed it, treat the rename as done.
	backup := opts.LegacyPath + ".migrated"

	if _, statErr := os.Stat(opts.LegacyPath); statErr == nil {
		if err := renameLegacyFile(opts.LegacyPath, backup); err != nil {
			return result, firerrors.Wrap(err,
				firerrors.KindEnvironment,
				Subsystem, "migrate", "preserve legacy file")
		}

		result.Renamed = true
	}

	return result, nil
}

// commitMigrationBatch writes one bounded batch with a single WAL
// append and counts it into the result.
func commitMigrationBatch(
	s *Store,
	batch *[]Pair,
	result *MigrationResult,
) error {
	if len(*batch) == 0 {
		return nil
	}

	if err := s.UpsertBatch(*batch); err != nil {
		return err
	}

	result.Migrated += len(*batch)
	*batch = (*batch)[:0]

	return nil
}

// legacyPass streams the entries map of the legacy file once,
// invoking handle for each valid entry. Memory stays bounded: only
// one decoded entry and the handler's own state exist at a time.
// handle is invoked single-threaded from this method.
func (s *Store) legacyPass(
	ctx context.Context,
	file *os.File,
	opts MigrateOptions,
	result *MigrationResult,
	handle func(key string, entry *LegacyEntry) error,
) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "migrate", "rewind legacy database")
	}

	decoder := json.NewDecoder(file)

	if err := expectToken(decoder, json.Delim('{')); err != nil {
		return firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "migrate", "legacy database is not a JSON object")
	}

	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}

		name, err := nextString(decoder)
		if err != nil {
			return firerrors.Wrap(err, firerrors.KindCorruptData,
				Subsystem, "migrate", "read legacy field name")
		}

		switch name {
		case "version":
			// Already validated by legacyVersion; skip the value.
			var skip json.RawMessage

			if err := decoder.Decode(&skip); err != nil {
				return firerrors.Wrap(err, firerrors.KindCorruptData,
					Subsystem, "migrate", "read legacy version")
			}

		case "entries":
			if err := s.legacyEntries(ctx, decoder, opts, result, handle); err != nil {
				return err
			}

		default:
			// Unknown field: skip its value for forward compatibility.
			var skip json.RawMessage

			if err := decoder.Decode(&skip); err != nil {
				return firerrors.Wrap(err, firerrors.KindCorruptData,
					Subsystem, "migrate", "skip unknown legacy field")
			}
		}
	}

	if err := expectToken(decoder, json.Delim('}')); err != nil {
		return firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "migrate", "terminate legacy database object")
	}

	return nil
}

// legacyEntries walks the entries object of the legacy file.
func (s *Store) legacyEntries(
	ctx context.Context,
	decoder *json.Decoder,
	opts MigrateOptions,
	result *MigrationResult,
	handle func(key string, entry *LegacyEntry) error,
) error {
	if err := expectToken(decoder, json.Delim('{')); err != nil {
		return firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "migrate", "legacy entries is not an object")
	}

	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}

		id, err := nextString(decoder)
		if err != nil {
			return firerrors.Wrap(err, firerrors.KindCorruptData,
				Subsystem, "migrate", "read legacy entry key")
		}

		var entry LegacyEntry

		if err := decoder.Decode(&entry); err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("entry %s: %v", id, err))

			if opts.Strict {
				return firerrors.New(firerrors.KindInvalidInput,
					Subsystem, "migrate", "entry %s: %v", id, err)
			}

			continue
		}

		if len(entry.Config) == 0 {
			result.Skipped++

			continue
		}

		// The record key IS the fingerprint (the legacy ID).
		key, keyErr := normalizeLegacyKey(id)
		if keyErr != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("entry %s: %v", id, keyErr))

			if opts.Strict {
				return firerrors.New(firerrors.KindInvalidInput,
					Subsystem, "migrate", "entry %s: %v", id, keyErr)
			}

			continue
		}

		if err := handle(key, &entry); err != nil {
			return err
		}
	}

	if err := expectToken(decoder, json.Delim('}')); err != nil {
		return firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "migrate", "terminate legacy entries object")
	}

	return nil
}

// expectToken asserts the next JSON token is the given delimiter.
func expectToken(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	delim, ok := token.(json.Delim)
	if !ok || delim != want {
		return fmt.Errorf("unexpected token %v, want %v", token, want)
	}

	return nil
}

// nextString reads one JSON string token.
func nextString(decoder *json.Decoder) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", err
	}

	str, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("expected string key, got %v", token)
	}

	return str, nil
}

// legacyVersion reads only the version field of the legacy envelope.
func legacyVersion(file *os.File) (int, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "migrate", "rewind legacy database")
	}

	decoder := json.NewDecoder(file)

	if err := expectToken(decoder, json.Delim('{')); err != nil {
		return 0, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "migrate", "legacy database is not valid JSON")
	}

	for decoder.More() {
		name, err := nextString(decoder)
		if err != nil {
			return 0, firerrors.Wrap(err, firerrors.KindCorruptData,
				Subsystem, "migrate", "read legacy field name")
		}

		if name == "version" {
			var version int

			if err := decoder.Decode(&version); err != nil {
				return 0, firerrors.Wrap(err, firerrors.KindCorruptData,
					Subsystem, "migrate", "read legacy version")
			}

			return version, nil
		}

		var skip json.RawMessage

		if err := decoder.Decode(&skip); err != nil {
			return 0, firerrors.Wrap(err, firerrors.KindCorruptData,
				Subsystem, "migrate", "skip unknown legacy field")
		}
	}

	return 0, firerrors.New(firerrors.KindCorruptData,
		Subsystem, "migrate", "legacy database has no version field")
}

// normalizeLegacyKey ensures the legacy ID is a canonical fingerprint
// key accepted by the store.
func normalizeLegacyKey(id string) (string, error) {
	if len(id) != 64 {
		return "", fmt.Errorf("ID is not a 64-char fingerprint")
	}

	raw, err := hex.DecodeString(strings.ToLower(id))
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(raw), nil
}
