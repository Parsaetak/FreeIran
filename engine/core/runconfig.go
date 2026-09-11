package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// RunConfig owns one temporary runtime-configuration directory and
// the configuration file written into it.
//
// Ownership rules (the Windows guarantee):
//   - the file is created with 0600 permissions inside a 0700
//     directory under the system temp root;
//   - the owning core process is stopped BEFORE Cleanup removes the
//     file, so no open handle can block deletion;
//   - removal retries a bounded number of times to absorb Windows
//     sharing-violation latency;
//   - Cleanup is idempotent and safe to call twice.
type RunConfig struct {
	dir  string
	path string
}

// NewRunConfig creates the temporary runtime directory. When workDir
// is empty a fresh directory is created under os.TempDir(); otherwise
// workDir is used directly (tests).
func NewRunConfig(workDir, backendName string) (*RunConfig, error) {
	dir := workDir

	if dir == "" {
		suffix, err := randomSuffix()
		if err != nil {
			return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "runconfig", "generate directory suffix")
		}

		dir = filepath.Join(os.TempDir(),
			fmt.Sprintf("freeiran-%s-%s", sanitizeName(backendName), suffix))

		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "runconfig", "create runtime dir %s", dir)
		}
	}

	return &RunConfig{dir: dir}, nil
}

// Path returns the configuration file path (set by Write).
func (rc *RunConfig) Path() string {
	if rc == nil {
		return ""
	}

	return rc.path
}

// Dir returns the runtime directory.
func (rc *RunConfig) Dir() string {
	if rc == nil {
		return ""
	}

	return rc.dir
}

// Write persists a backend-generated runtime configuration with
// secure permissions. The file name comes from the document so
// backends control the extension.
func (rc *RunConfig) Write(doc RuntimeConfig) error {
	if rc == nil {
		return firerrors.New(firerrors.KindFatal, Subsystem, "runconfig",
			"nil run config")
	}

	if len(doc.Data) == 0 {
		return firerrors.New(firerrors.KindConfiguration, Subsystem,
			"runconfig", "runtime configuration document is empty")
	}

	name := doc.FileName

	if name == "" {
		name = "config.json"
	}

	name = filepath.Base(sanitizeName(name))

	rc.path = filepath.Join(rc.dir, name)

	// 0600: the document carries credentials; only the owner may
	// read it. On Windows the mode is advisory but the file lives in
	// a 0700 directory under the user's temp root.
	if err := os.WriteFile(rc.path, doc.Data, 0o600); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "runconfig", "write %s", rc.path)
	}

	return nil
}

// Cleanup removes the runtime configuration file and its directory
// when FreeIran owns the directory. Failures retry a bounded number
// of times to absorb transient Windows sharing violations; the final
// error is returned (logged, never fatal to shutdown).
func (rc *RunConfig) Cleanup() error {
	if rc == nil {
		return nil
	}

	if rc.path == "" && rc.dir == "" {
		return nil
	}

	var firstErr error

	// Remove the config file (stopped process => no open handle).
	if rc.path != "" {
		if err := removeWithRetry(rc.path); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Remove the directory only when it sits under the temp root
	// (workDir overrides are caller-owned, e.g. test directories).
	if rc.dir != "" && isManagedDir(rc.dir) {
		if err := removeWithRetry(rc.dir); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	rc.path = ""
	rc.dir = ""

	return firstErr
}

// removeWithRetry deletes a path with bounded retries: Windows may
// report a sharing violation for a short window after process exit.
func removeWithRetry(path string) error {
	const attempts = 3

	var err error

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(25 * time.Millisecond)
		}

		err = os.Remove(path)

		if err == nil || os.IsNotExist(err) {
			return nil
		}
	}

	return err
}

// isManagedDir reports whether the directory was created by
// NewRunConfig under the system temp root (prefix match).
func isManagedDir(dir string) bool {
	if dir == "" {
		return false
	}

	base := filepath.Base(dir)

	return len(base) > len("freeiran-x-") &&
		base[:len("freeiran-")] == "freeiran-"
}

// randomSuffix generates 8 hex characters for directory names.
func randomSuffix() (string, error) {
	buf := make([]byte, 4)

	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf), nil
}

// sanitizeName reduces an arbitrary backend/file name to a
// filesystem-safe token.
func sanitizeName(name string) string {
	safe := make([]rune, 0, len(name))

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			safe = append(safe, r)
		default:
			safe = append(safe, '-')
		}
	}

	if len(safe) == 0 {
		return "core"
	}

	return string(safe)
}
