// workspace.go resolves FreeIran's single Workspace Root.
//
// v0.9.2 runtime model: EVERYTHING FreeIran creates or needs lives
// below one explicit root — by default the directory containing the
// FreeIran executable:
//
//	FreeIran/
//	├── FreeIran.exe            (or FreeIran on Linux)
//	├── portable.marker
//	├── config/                 sources.json, settings, workspace status
//	├── data/                   chunked store (chunks/, wal/, meta, index)
//	├── cache/
//	├── logs/
//	├── cores/                  managed protocol cores (+ wintun)
//	├── runtime/                short-lived temporary files, always cleaned
//	└── docs/, deployment/      static release content
//
// No normal runtime state is silently split across %APPDATA%,
// %LOCALAPPDATA%, XDG data/cache or the system temp directory. A
// developer/operator override (FREEIRAN_HOME) relocates the root
// explicitly; data from pre-0.9.2 installs is migrated once by
// workspace_migrate.go — never abandoned and never silently duplicated.
package system

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// workspaceOverrideEnv is the explicit developer/operator override for
// the workspace root. When set (and non-empty) it wins over the
// executable-directory default so CI, tests and custom deployments can
// relocate the whole tree deterministically.
const workspaceOverrideEnv = "FREEIRAN_HOME"

// skipMigrationEnv disables the one-time legacy-location discovery
// (workspace_migrate.go) for automated deployments that must never
// touch the old per-user directories.
const skipMigrationEnv = "FREEIRAN_SKIP_MIGRATION"

// cachedWorkspaceRoot memoizes the resolved root. Resolution is
// deterministic per process (env + executable path do not change), so
// the first result is kept and reused — every subsystem observes the
// exact same root for the whole lifetime of the application.
var cachedWorkspaceRoot atomic.Value // string

// WorkspaceRoot returns FreeIran's single workspace root directory.
//
// Resolution order:
//  1. $FREEIRAN_HOME (explicit override; expanded to absolute form)
//  2. the directory containing the running FreeIran executable
//     (portable deployments AND regular installs — one model)
//
// The result is resolved once and memoized. WorkspaceRoot never fails:
// when os.Executable is unavailable (extremely rare) it falls back to
// the working directory so diagnostics can still name a root.
func WorkspaceRoot() string {
	if cached := cachedWorkspaceRoot.Load(); cached != nil {
		if root, ok := cached.(string); ok && root != "" {
			return root
		}
	}

	root := resolveWorkspaceRoot()
	cachedWorkspaceRoot.Store(root)

	return root
}

func resolveWorkspaceRoot() string {
	if override := strings.TrimSpace(os.Getenv(workspaceOverrideEnv)); override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			return filepath.Clean(abs)
		}

		return filepath.Clean(override)
	}

	exe, err := os.Executable()
	if err != nil || filepath.Dir(exe) == "" {
		// Last resort so a root always exists for diagnostics; boot
		// validation below will report any usability problem.
		if wd, wdErr := os.Getwd(); wdErr == nil {
			return wd
		}

		return "."
	}

	return filepath.Dir(exe)
}

// WorkspaceLayout returns the canonical directory layout under the
// workspace root. It is the single path authority for every persistent
// FreeIran subsystem: store, logs, cache, core manager, runtime temp
// files, diagnostics, source metadata, deployment metadata and
// application settings all derive their locations from here.
func WorkspaceLayout() DirNames {
	return Layout(WorkspaceRoot())
}

// WorkspaceWritable reports whether files can be created in root. The
// probe creates and removes a uniquely named marker file so read-only
// deployments are detected before any subsystem tries to write.
func WorkspaceWritable(root string) error {
	if root == "" {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "workspace", "workspace root is empty")
	}

	probe, err := os.CreateTemp(root, ".workspace-probe-*")
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "workspace", "workspace %s is not writable", root)
	}

	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)

	return nil
}

// EnsureWorkspace creates the full workspace directory tree and
// validates that the root is usable. It returns a descriptive error
// (with the fix) when the workspace cannot be used, so startup
// diagnostics can surface an actionable message instead of a cascade
// of per-subsystem failures.
func EnsureWorkspace() (DirNames, error) {
	root := WorkspaceRoot()

	if err := ensureWorkspaceDirs(root); err != nil {
		return DirNames{}, err
	}

	if err := WorkspaceWritable(root); err != nil {
		return DirNames{}, firerrors.New(firerrors.KindEnvironment,
			Subsystem, "workspace",
			"workspace %s is read-only: FreeIran cannot store data next to the "+
				"executable. Move FreeIran to a writable folder (or extract the "+
				"release ZIP somewhere writable), or set %s to a writable path",
			root, workspaceOverrideEnv)
	}

	return WorkspaceLayout(), nil
}

func ensureWorkspaceDirs(root string) error {
	if root == "" {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "workspace", "workspace root is empty")
	}

	layout := Layout(root)

	for _, dir := range []string{
		layout.Root, layout.Data, layout.Cache, layout.Logs,
		layout.Cores, layout.Config, layout.Runtime,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "workspace", "create %s", dir)
		}
	}

	return nil
}

// WorkspaceSkipsMigration reports whether the one-time legacy-location
// migration is disabled by environment (automated deployments).
func WorkspaceSkipsMigration() bool {
	switch strings.TrimSpace(os.Getenv(skipMigrationEnv)) {
	case "1", "true", "TRUE", "True", "yes", "on":
		return true
	}

	return false
}

// DefaultBaseDir returns the workspace root. It is retained as the
// historical name used by engine/app; all callers now share the single
// workspace model (v0.9.2) instead of per-user OS directories.
func DefaultBaseDir() string {
	return WorkspaceRoot()
}

// CacheBaseDir returns the workspace root: the cache lives under the
// same root as every other subsystem (no split cache/data roots).
func CacheBaseDir() string {
	return WorkspaceRoot()
}

// DescribeWorkspace renders the workspace layout for logs and
// diagnostics. Paths are absolute; no secrets are involved.
func DescribeWorkspace(layout DirNames) string {
	return fmt.Sprintf("root=%s data=%s cache=%s logs=%s cores=%s runtime=%s config=%s",
		layout.Root, layout.Data, layout.Cache, layout.Logs,
		layout.Cores, layout.Runtime, layout.Config)
}
