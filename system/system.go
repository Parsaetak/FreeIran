// Package system provides FreeIran's local system integration layer:
// filesystem layout, process lifecycle management for protocol cores,
// executable discovery, network capability checks and platform
// information.
//
// Platform-specific behaviour is isolated in platform files guarded by
// build tags so the rest of the engine stays OS-neutral.
package system

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// Subsystem identifies the system layer in structured errors.
const Subsystem = "system"

// DirNames enumerates the application directory layout. Every FreeIran
// artifact lives under one platform-specific base directory.
type DirNames struct {
	Root   string
	Data   string
	Cache  string
	Logs   string
	Cores  string
	Config string
}

// Layout resolves the application directories under base.
func Layout(base string) DirNames {
	return DirNames{
		Root:   base,
		Data:   filepath.Join(base, "data"),
		Cache:  filepath.Join(base, "cache"),
		Logs:   filepath.Join(base, "logs"),
		Cores:  filepath.Join(base, "cores"),
		Config: filepath.Join(base, "config"),
	}
}

// EnsureLayout creates the directory tree and returns it.
func EnsureLayout(base string) (DirNames, error) {
	if base == "" {
		return DirNames{}, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "layout", "base directory is empty")
	}

	layout := Layout(base)

	for _, dir := range []string{
		layout.Root, layout.Data, layout.Cache,
		layout.Logs, layout.Cores, layout.Config,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return layout, firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "layout", "create %s", dir)
		}
	}

	return layout, nil
}

// Info describes the runtime environment.
type Info struct {
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	GoVersion    string `json:"go_version"`
	NumCPU       int    `json:"num_cpu"`
	Hostname     string `json:"hostname"`
	PlatformNote string `json:"platform_note"`
}

// GetInfo collects platform information.
func GetInfo() Info {
	hostname, _ := os.Hostname()

	return Info{
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		GoVersion:    runtime.Version(),
		NumCPU:       runtime.NumCPU(),
		Hostname:     hostname,
		PlatformNote: platformNote(),
	}
}

// CoreLocator discovers installed protocol-core executables.
type CoreLocator struct {
	mu        sync.RWMutex
	coreDir   string
	lookPath  func(string) (string, error)
	extraDirs []string
}

// NewCoreLocator creates a locator that searches the system PATH, the
// application cores directory and any extra directories.
func NewCoreLocator(coreDir string, extraDirs ...string) *CoreLocator {
	return &CoreLocator{
		coreDir:   coreDir,
		lookPath:  execLookPath,
		extraDirs: extraDirs,
	}
}

// CoreBinary describes one discovered protocol core.
type CoreBinary struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

// WellKnownCores are the protocol engines FreeIran integrates with.
var WellKnownCores = []string{"xray", "sing-box", "wireguard", "wg"}

// Discover finds a core executable by name and queries its version.
func (l *CoreLocator) Discover(ctx context.Context, name string) (CoreBinary, error) {
	if l == nil {
		return CoreBinary{}, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "discover", "locator is nil")
	}

	candidates := []string{l.coreDir}
	candidates = append(candidates, l.extraDirs...)

	for _, dir := range candidates {
		if dir == "" {
			continue
		}

		path := filepath.Join(dir, executableName(name))

		if fileExists(path) {
			return CoreBinary{
				Name:    name,
				Path:    path,
				Version: queryCoreVersion(ctx, path),
			}, nil
		}
	}

	path, err := l.lookPath(name)
	if err == nil && path != "" {
		return CoreBinary{
			Name:    name,
			Path:    path,
			Version: queryCoreVersion(ctx, path),
		}, nil
	}

	return CoreBinary{}, firerrors.New(firerrors.KindDependencyUnavailable,
		Subsystem, "discover", "core %q not found", name)
}

// DiscoverAll locates every well-known core, skipping missing ones.
func (l *CoreLocator) DiscoverAll(ctx context.Context) []CoreBinary {
	found := make([]CoreBinary, 0, len(WellKnownCores))

	for _, name := range WellKnownCores {
		if binary, err := l.Discover(ctx, name); err == nil {
			found = append(found, binary)
		}
	}

	return found
}

// ProcessSpec describes how to launch a protocol core.
type ProcessSpec struct {
	Name    string
	Path    string
	Args    []string
	Env     []string
	WorkDir string
}

// ManagedProcess is one supervised protocol-core process.
type ManagedProcess struct {
	spec      ProcessSpec
	StartedAt time.Time

	mu      sync.Mutex
	stopped bool
	exited  chan struct{}
	err     error
	pid     int
}

// Start launches a process and begins supervising it.
func Start(ctx context.Context, spec ProcessSpec) (*ManagedProcess, error) {
	if spec.Path == "" {
		return nil, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "start", "executable path is empty")
	}

	if _, err := os.Stat(spec.Path); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindDependencyUnavailable,
			Subsystem, "start", "core binary %q", spec.Path)
	}

	proc, err := launchProcess(ctx, spec)
	if err != nil {
		return nil, err
	}

	return proc, nil
}

// Wait blocks until the process exits or the context is cancelled.
func (m *ManagedProcess) Wait(ctx context.Context) error {
	if m == nil {
		return firerrors.New(firerrors.KindFatal, Subsystem, "wait", "nil process")
	}

	select {
	case <-m.exited:
		return m.err

	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop terminates the process: polite shutdown first, forced kill
// after the grace period.
func (m *ManagedProcess) Stop(grace time.Duration) error {
	if m == nil {
		return nil
	}

	m.mu.Lock()

	if m.stopped {
		m.mu.Unlock()

		return nil
	}

	m.stopped = true
	m.mu.Unlock()

	return terminateProcess(m, grace)
}

// Running reports whether the process is still alive.
func (m *ManagedProcess) Running() bool {
	if m == nil {
		return false
	}

	select {
	case <-m.exited:
		return false

	default:
		return true
	}
}

// PID returns the operating-system process identifier.
func (m *ManagedProcess) PID() int {
	if m == nil {
		return 0
	}

	return m.pid
}

// Redact replaces secret-looking substrings in diagnostic text. Core
// logs can echo credentials; the system layer never forwards them.
func Redact(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}

		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}

	return text
}

func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}
