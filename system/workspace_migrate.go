// workspace_migrate.go implements the one-time legacy-location
// discovery and migration path (v0.9.2).
//
// Pre-0.9.2 installs stored state under per-user OS directories
// (%APPDATA%\FreeIran, %LOCALAPPDATA%\FreeIran, XDG data/cache dirs).
// The workspace model does NOT abandon that data:
//
//  1. check the new workspace (fresh? no authoritative data yet?)
//  2. detect existing legacy FreeIran data
//  3. migrate it into the workspace (copy, never move)
//  4. verify the copied bytes and file counts
//  5. preserve the source untouched until migration is confirmed
//  6. record the migration status under config/workspace.json
//  7. never silently duplicate: migration runs only when the
//     workspace has no authoritative data of its own
//
// The new workspace becomes authoritative after a verified copy. The
// legacy directories are left in place (they may still be needed by an
// older parallel install); cleanup of a confirmed legacy location is a
// separate, explicit user decision. Automated deployments can disable
// the whole path with FREEIRAN_SKIP_MIGRATION=1.
package system

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// workspaceStatusFile records the one-time workspace migration.
const workspaceStatusFile = "workspace.json"

// WorkspaceStatus is the persisted migration record.
type WorkspaceStatus struct {
	// Version is the status format version.
	Version int `json:"version"`

	// Migrated reports whether a legacy location was copied in.
	Migrated bool `json:"migrated"`

	// Source is the legacy root the data was copied from.
	Source string `json:"source,omitempty"`

	// MigratedAt is the completion time (UTC, RFC 3339).
	MigratedAt string `json:"migrated_at,omitempty"`

	// Files and Bytes describe the verified copy.
	Files int   `json:"files,omitempty"`
	Bytes int64 `json:"bytes,omitempty"`

	// Skipped explains why no migration was attempted.
	Skipped string `json:"skipped,omitempty"`
}

// MigrationReport summarizes one migration attempt.
type MigrationReport struct {
	// Performed reports whether a copy was executed and verified.
	Performed bool

	// Source is the legacy root ("" when nothing was migrated).
	Source string

	// Files and Bytes count the verified copy.
	Files int
	Bytes int64

	// Reason explains why migration was skipped (empty when done).
	Reason string
}

// LegacyLocations lists the pre-0.9.2 per-user roots in priority
// order. Only locations that exist on disk matter; the list is
// diagnostic and migration input, never a runtime path authority.
func LegacyLocations() []string {
	var locations []string

	add := func(dir string) {
		if dir != "" {
			locations = append(locations, dir)
		}
	}

	appData := os.Getenv("APPDATA")

	if appData != "" {
		add(filepath.Join(appData, "FreeIran"))
	}

	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		add(filepath.Join(localAppData, "FreeIran"))
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if appData == "" {
			add(filepath.Join(home, "AppData", "Roaming", "FreeIran"))
		}

		add(filepath.Join(home, "AppData", "Local", "FreeIran"))
		add(filepath.Join(home, ".local", "share", "FreeIran"))
		add(filepath.Join(home, ".cache", "FreeIran"))
	}

	return locations
}

// DetectLegacyWorkspace finds the first legacy root that still holds
// authoritative FreeIran data (a data/ store or a config/ sources
// file). It never modifies anything.
func DetectLegacyWorkspace() (string, bool) {
	for _, candidate := range LegacyLocations() {
		if hasAuthoritativeData(candidate) {
			return candidate, true
		}
	}

	return "", false
}

// hasAuthoritativeData reports whether root contains data that a
// pre-0.9.2 install would recognize as its own.
func hasAuthoritativeData(root string) bool {
	if info, err := os.Stat(filepath.Join(root, "data", "store.meta")); err == nil && !info.IsDir() {
		return true
	}

	if info, err := os.Stat(filepath.Join(root, "data", "db.json")); err == nil && !info.IsDir() {
		return true
	}

	if info, err := os.Stat(filepath.Join(root, "config", "sources.json")); err == nil && !info.IsDir() {
		return true
	}

	return false
}

// MigrateLegacyWorkspace copies a legacy root into the workspace and
// verifies the result. The source is never modified or removed.
//
// Copied trees: config, data, cores, cache, providers. Runtime and logs are NOT
// copied — they are reconstructable by definition, and importing old
// logs into the new workspace would blur the log surface. The copy is
// verified per file (byte count) before the status record is written.
func MigrateLegacyWorkspace(source string, log func(format string, args ...any)) (MigrationReport, error) {
	report := MigrationReport{}

	if source == "" {
		report.Reason = "no legacy location"

		return report, nil
	}

	workspace := WorkspaceRoot()

	if hasAuthoritativeData(workspace) {
		report.Reason = "workspace already holds authoritative data"

		return report, nil
	}

	if err := ensureWorkspaceDirs(workspace); err != nil {
		return report, err
	}

	say := func(format string, args ...any) {
		if log != nil {
			log(format, args...)
		}
	}

	say("migrating legacy workspace %s → %s", source, workspace)

	var (
		files int
		bytes int64
	)

	for _, tree := range []string{"config", "data", "cores", "cache", "providers"} {
		src := filepath.Join(source, tree)
		dst := filepath.Join(workspace, tree)

		if _, err := os.Stat(src); err != nil {
			continue
		}

		f, b, err := copyTree(src, dst)
		if err != nil {
			return MigrationReport{}, firerrors.Wrap(err,
				firerrors.KindEnvironment, Subsystem, "migrate",
				"copy %s", tree)
		}

		files += f
		bytes += b

		say("migrated %s: %d files, %s", tree, f, humanBytes(b))
	}

	// Verify: every regular file under the source trees must exist in
	// the workspace with the same size. The copy is byte-count verified
	// (checksums would double the read cost; a size mismatch detects
	// truncation, and the store's own checksummed chunks detect the
	// rest at open time).
	verified, verr := verifyCopy(source, workspace)
	if verr != nil {
		return MigrationReport{}, firerrors.Wrap(verr,
			firerrors.KindEnvironment, Subsystem, "migrate",
			"verify copied data")
	}

	if verified != files {
		return MigrationReport{}, firerrors.New(firerrors.KindEnvironment,
			Subsystem, "migrate",
			"verification mismatch: copied %d files, verified %d", files, verified)
	}

	status := WorkspaceStatus{
		Version:    1,
		Migrated:   true,
		Source:     source,
		MigratedAt: time.Now().UTC().Format(time.RFC3339),
		Files:      files,
		Bytes:      bytes,
	}

	if err := saveWorkspaceStatus(status); err != nil {
		return MigrationReport{}, err
	}

	say("workspace migration verified: %d files, %s (source preserved)",
		files, humanBytes(bytes))

	return MigrationReport{
		Performed: true,
		Source:    source,
		Files:     files,
		Bytes:     bytes,
	}, nil
}

// EnsureWorkspaceMigrated runs the one-time discovery/migration for a
// freshly defaulted workspace. It is deterministic:
//
//   - explicit override roots participate (FREEIRAN_HOME), so a
//     relocated workspace still finds its data;
//   - the migration is skipped when the workspace already holds
//     authoritative data, when the environment disables it, and when
//     no legacy location holds data.
//
// Every outcome is recorded in config/workspace.json so diagnostics
// can show exactly what happened.
func EnsureWorkspaceMigrated(log func(format string, args ...any)) (MigrationReport, error) {
	report := MigrationReport{}

	if WorkspaceSkipsMigration() {
		report.Reason = "disabled by " + skipMigrationEnv
		saveWorkspaceStatus(WorkspaceStatus{Version: 1, Skipped: report.Reason})

		return report, nil
	}

	if hasAuthoritativeData(WorkspaceRoot()) {
		report.Reason = "workspace already holds authoritative data"
		saveWorkspaceStatus(WorkspaceStatus{Version: 1, Skipped: report.Reason})

		return report, nil
	}

	source, found := DetectLegacyWorkspace()
	if !found {
		report.Reason = "no legacy data found"
		saveWorkspaceStatus(WorkspaceStatus{Version: 1, Skipped: report.Reason})

		return report, nil
	}

	return MigrateLegacyWorkspace(source, log)
}

// LoadWorkspaceStatus reads the persisted migration record.
func LoadWorkspaceStatus() WorkspaceStatus {
	raw, err := os.ReadFile(filepath.Join(WorkspaceRoot(), "config", workspaceStatusFile))
	if err != nil {
		return WorkspaceStatus{Version: 1}
	}

	var status WorkspaceStatus

	if err := json.Unmarshal(raw, &status); err != nil {
		return WorkspaceStatus{Version: 1}
	}

	return status
}

// saveWorkspaceStatus persists the migration record atomically.
func saveWorkspaceStatus(status WorkspaceStatus) error {
	raw, err := json.MarshalIndent(&status, "", "  ")
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindFatal,
			Subsystem, "migrate", "encode workspace status")
	}

	dir := filepath.Join(WorkspaceRoot(), "config")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "migrate", "create config directory")
	}

	return WriteFileAtomic(filepath.Join(dir, workspaceStatusFile), raw, 0o600)
}

// copyTree recursively copies one directory tree and returns the
// number of files and bytes written.
func copyTree(src, dst string) (int, int64, error) {
	var (
		files int
		bytes int64
	)

	err := filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}

		if !entry.Type().IsRegular() {
			return nil // skip symlinks and irregular entries
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if err := copyFile(path, target, info.Mode().Perm()); err != nil {
			return err
		}

		files++
		bytes += info.Size()

		return nil
	})

	return files, bytes, err
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}

	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}

	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	if err := out.Sync(); err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}

// verifyCopy checks that every regular file under the copied trees of
// src exists in dst with the same size.
func verifyCopy(src, dst string) (int, error) {
	verified := 0

	err := filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		// The walk root itself: descend into the trees.
		if rel == "." {
			return nil
		}

		// Only the trees the migration copies are verified.
		top := rel
		if i := indexSep(rel); i >= 0 {
			top = rel[:i]
		}

		switch top {
		case "config", "data", "cores", "cache":
		default:
			if entry.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		dstInfo, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			return fmt.Errorf("copied file missing: %s", rel)
		}

		if dstInfo.Size() != info.Size() {
			return fmt.Errorf("size mismatch for %s: %d != %d",
				rel, dstInfo.Size(), info.Size())
		}

		verified++

		return nil
	})

	return verified, err
}

// indexSep returns the index of the first path separator in p, or -1.
func indexSep(p string) int {
	for i := 0; i < len(p); i++ {
		if p[i] == os.PathSeparator {
			return i
		}
	}

	return -1
}

// humanBytes renders a byte count for logs and status records.
func humanBytes(n int64) string {
	const unit = 1024

	switch {
	case n >= unit*unit*unit:
		return fmt.Sprintf("%.1f GiB", float64(n)/(unit*unit*unit))
	case n >= unit*unit:
		return fmt.Sprintf("%.1f MiB", float64(n)/(unit*unit))
	case n >= unit:
		return fmt.Sprintf("%.1f KiB", float64(n)/unit)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
