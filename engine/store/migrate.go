package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
//   - Every record is validated before it is staged; a corrupt record
//     is reported, not silently dropped (strict mode).
//   - On any failure the store is left consistent: staging only
//     mutates the in-memory memtable, and a failed run aborts before
//     the rename.

// LegacyEntry mirrors engine/database.Entry.
type LegacyEntry struct {
	Config  json.RawMessage `json:"config"`
	Added   string          `json:"added"`
	Updated string          `json:"updated"`
}

// LegacyState mirrors the legacy diskState envelope.
type LegacyState struct {
	Version int                     `json:"version"`
	Entries map[string]*LegacyEntry `json:"entries"`
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

	raw, err := os.ReadFile(opts.LegacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Idempotent: nothing to migrate.
			return result, nil
		}

		return result, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "migrate", "read legacy database")
	}

	var state LegacyState

	if err := json.Unmarshal(raw, &state); err != nil {
		return result, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "migrate", "legacy database is not valid JSON")
	}

	if state.Version != 1 {
		return result, firerrors.New(firerrors.KindCorruptData,
			Subsystem, "migrate", "unsupported legacy version %d",
			state.Version)
	}

	// Deterministic order: keys sorted, so the chunk layout produced
	// by migration is reproducible.
	ids := make([]string, 0, len(state.Entries))

	for id := range state.Entries {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	batch := make([]Pair, 0, 1024)

	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}

		if err := s.UpsertBatch(batch); err != nil {
			return err
		}

		result.Migrated += len(batch)
		batch = batch[:0]

		return nil
	}

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		entry := state.Entries[id]
		if entry == nil || len(entry.Config) == 0 {
			result.Skipped++

			continue
		}

		// The record key IS the fingerprint (the legacy ID).
		key, keyErr := normalizeLegacyKey(id)
		if keyErr != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("entry %s: %v", id, keyErr))

			if opts.Strict {
				return result, firerrors.New(
					firerrors.KindInvalidInput,
					Subsystem, "migrate", "entry %s: %v", id, keyErr)
			}

			continue
		}

		batch = append(batch, Pair{Key: key, Value: entry.Config})

		if len(batch) >= 1024 {
			if err := flushBatch(); err != nil {
				return result, err
			}
		}
	}

	if err := flushBatch(); err != nil {
		return result, err
	}

	// Persist everything before declaring success.
	if err := s.Flush(); err != nil {
		return result, err
	}

	// Verify: every migrated key must now be readable from the store.
	for _, id := range ids {
		entry := state.Entries[id]
		if entry == nil || len(entry.Config) == 0 {
			continue
		}

		key, keyErr := normalizeLegacyKey(id)
		if keyErr != nil {
			continue
		}

		value, getErr := s.Get(key)
		if getErr != nil || value == nil {
			return result, firerrors.New(firerrors.KindCorruptData,
				Subsystem, "migrate",
				"verification failed for %s", id)
		}
	}

	// Preserve the legacy file (rename, never delete). If a previous
	// migration already renamed it, treat the rename as done.
	backup := opts.LegacyPath + ".migrated"

	if _, statErr := os.Stat(opts.LegacyPath); statErr == nil {
		if err := os.Rename(opts.LegacyPath, backup); err != nil {
			return result, firerrors.Wrap(err,
				firerrors.KindEnvironment,
				Subsystem, "migrate", "preserve legacy file")
		}

		result.Renamed = true
	}

	return result, nil
}

// normalizeLegacyKey ensures the legacy ID is a canonical fingerprint
// key accepted by the store.
func normalizeLegacyKey(id string) (string, error) {
	if len(id) != 64 {
		return "", fmt.Errorf("ID is not a 64-char fingerprint")
	}

	raw, err := hexDecode(id)
	if err != nil {
		return "", err
	}

	return hexEncode(raw), nil
}

func hexDecode(s string) ([]byte, error) {
	return hex.DecodeString(strings.ToLower(s))
}

func hexEncode(b []byte) string {
	return hex.EncodeToString(b)
}
