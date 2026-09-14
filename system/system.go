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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// Subsystem identifies the system layer in structured errors.
const Subsystem = "system"

// Lifecycle timing constants for supervised processes.
const (
	// pipeDrainDelay bounds how long the child's stdout/stderr pipes
	// may stay open after process exit or context cancellation before
	// exec forces them closed. It makes cmd.Wait — and therefore the
	// writers-complete-before-exited guarantee — deterministic even
	// if a stray descendant briefly holds the pipe write end.
	pipeDrainDelay = 5 * time.Second

	// hardKillDeadline bounds the wait for process death after a
	// forced termination (job terminate / tree kill / group kill).
	hardKillDeadline = 10 * time.Second
)

// DirNames enumerates the application directory layout. Every FreeIran
// artifact lives under one base directory — since v0.9.2 the single
// Workspace Root (system/workspace.go), never per-user OS directories.
type DirNames struct {
	Root    string
	Data    string
	Cache   string
	Logs    string
	Cores   string
	Config  string
	Runtime string
}

// Layout resolves the application directories under base.
func Layout(base string) DirNames {
	return DirNames{
		Root:    base,
		Data:    filepath.Join(base, "data"),
		Cache:   filepath.Join(base, "cache"),
		Logs:    filepath.Join(base, "logs"),
		Cores:   filepath.Join(base, "cores"),
		Config:  filepath.Join(base, "config"),
		Runtime: filepath.Join(base, "runtime"),
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
		layout.Logs, layout.Cores, layout.Config, layout.Runtime,
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
// v2ray is the actively maintained V2Fly edition (v2fly/v2ray-core),
// distinct from Xray-core.
var WellKnownCores = []string{"xray", "v2ray", "sing-box", "wireguard", "wg"}

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

	// Stdout and Stderr, when set, receive the captured process output
	// instead of /dev/null. Writers must be safe for concurrent use;
	// exec copies stdout and stderr through two concurrent goroutines,
	// and cmd.Wait joins both before the exited channel closes.
	Stdout io.Writer
	Stderr io.Writer
}

// ProcessState is the deterministic lifecycle state of a
// ManagedProcess.
//
// State transitions:
//
//	Running  → Stopping → Stopped     (Stop, forced or graceful)
//	Running  → Exited                (natural exit, any code)
//	Running  → Cancelled             (context cancellation)
//
// Launch failures never produce a ManagedProcess (Start returns an
// error instead); readiness/timeout failures are classified by the
// engine/core layer on top of these states.
type ProcessState int

const (
	// StateRunning: supervised and alive (or not yet observed to have
	// exited).
	StateRunning ProcessState = iota

	// StateStopping: Stop has been called and termination is in
	// progress.
	StateStopping

	// StateStopped: terminated by an explicit Stop call.
	StateStopped

	// StateCancelled: terminated because the launch context was
	// cancelled.
	StateCancelled

	// StateExited: exited on its own (exit code 0 or otherwise).
	StateExited
)

// String returns a stable, machine-readable state name.
func (s ProcessState) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateStopping:
		return "stopping"
	case StateStopped:
		return "stopped"
	case StateCancelled:
		return "cancelled"
	case StateExited:
		return "exited"
	}

	return "unknown"
}

// ProcessDiagnostics is the structured, exportable supervision
// snapshot for one managed process. It powers the runtime
// diagnostics surface (observability) and never includes captured
// process output or secrets.
type ProcessDiagnostics struct {
	Name            string    `json:"name"`
	PID             int       `json:"pid"`
	State           string    `json:"state"`
	JobBound        bool      `json:"job_bound"`
	SupervisionNote string    `json:"supervision_note,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	ExitCode        int       `json:"exit_code"`
	ExitError       string    `json:"exit_error,omitempty"`
	Alive           bool      `json:"alive"`
}

// ManagedProcess is one supervised protocol-core process.
//
// The supervision invariant — a protocol core must never become an
// unmanaged/orphaned process — is enforced by the platform layer:
// a kill-on-close job object on Windows (or the supervised
// process-tree fallback when job binding is restricted), and the
// process group on Unix. See process_windows.go / process_unix.go.
type ManagedProcess struct {
	spec      ProcessSpec
	StartedAt time.Time

	mu              sync.Mutex
	state           ProcessState
	stopped         bool
	job             *jobHandle
	jobBound        bool
	supervisionNote string

	exited   chan struct{}
	err      error
	exitCode int
	pid      int

	stopStarted bool
	stopDone    chan struct{}
	stopErr     error
}

// Start launches a process and begins supervising it.
//
// The executable path is resolved by the platform resolver: explicit
// paths (absolute or containing a separator) are validated with the
// same strictness as v0.7 — os.Stat must succeed and return a regular
// file; the only bare names accepted are known Windows system shell
// binaries, resolved through COMSPEC with a validated
// %SystemRoot%\System32 fallback, never through the current working
// directory or an inherited PATH.
func Start(ctx context.Context, spec ProcessSpec) (*ManagedProcess, error) {
	if spec.Path == "" {
		return nil, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "start", "executable path is empty")
	}

	resolved, err := resolveExecutable(spec.Path)
	if err != nil {
		return nil, err
	}

	spec.Path = resolved

	proc, err := launchProcess(ctx, spec)
	if err != nil {
		return nil, err
	}

	return proc, nil
}

// supervise wraps a started child in the ManagedProcess lifecycle:
// it records the binding state, starts the wait goroutine that
// classifies the exit, and never fails after the child exists (all
// degradation is captured in supervisionNote instead, so a process
// that was successfully created is NEVER leaked on a post-start
// error path).
func supervise(
	ctx context.Context,
	spec ProcessSpec,
	cmd *exec.Cmd,
	job *jobHandle,
	bound bool,
	note string,
) (*ManagedProcess, error) {
	proc := &ManagedProcess{
		spec:            spec,
		StartedAt:       time.Now().UTC(),
		exited:          make(chan struct{}),
		stopDone:        make(chan struct{}),
		pid:             cmd.Process.Pid,
		job:             job,
		jobBound:        bound,
		supervisionNote: note,
		exitCode:        -1,
		state:           StateRunning,
	}

	if !bound {
		logging.W("system", "job_fallback",
			"process %q (pid %d) launched without kernel job binding (%s); "+
				"deterministic supervised tree-kill cleanup is active",
			spec.Name, proc.pid, note)
	}

	go func() {
		err := cmd.Wait()

		state := StateExited

		switch {
		case err == nil:
			// Natural clean exit (regardless of context state).
		case proc.wasStopped():
			// Intentional termination: not a failure.
			state = StateStopped
			err = nil
		case ctx.Err() != nil:
			// The exit was driven by context cancellation: surface
			// the cancellation itself as the reason, not the kill's
			// exit-status artifact.
			state = StateCancelled
			err = ctx.Err()
		}

		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}

		proc.finish(state, err, code)
	}()

	return proc, nil
}

// finish records the terminal state and enforces the no-descendant
// invariant. Exactly one goroutine (the wait goroutine) calls it.
func (m *ManagedProcess) finish(state ProcessState, err error, code int) {
	m.mu.Lock()
	m.exitCode = code
	m.err = err

	if m.state == StateRunning {
		m.state = state
	}

	m.mu.Unlock()

	// The direct child is gone. Enforce the supervision invariant
	// BEFORE signalling exit observers: descendants may outlive a
	// politely-exited child (async signal delivery, or a grandchild
	// that ignores SIGTERM), so every exit path — natural, cancelled,
	// stopped — reaps the whole supervision domain.
	//
	//   Unix:            group SIGKILL (covers every member).
	//   Windows bound:   closeJob's kill-on-close (next call below).
	//   Windows fallback: Toolhelp32 process-tree kill.
	reapDescendants(m)

	// Closing the job handle is itself a kernel-level kill for any
	// member that somehow survived the reap.
	m.closeJob()

	close(m.exited)
}

// wasStopped reports whether Stop has been initiated.
func (m *ManagedProcess) wasStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.stopped
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
//
// Stop is idempotent, race-free and synchronizing: concurrent callers
// all wait for the SAME termination to complete and observe the same
// result; later calls return the recorded result immediately. The
// first caller's grace period is the one applied.
func (m *ManagedProcess) Stop(grace time.Duration) error {
	if m == nil {
		return nil
	}

	m.mu.Lock()

	if m.stopStarted {
		done := m.stopDone
		m.mu.Unlock()

		<-done

		m.mu.Lock()
		defer m.mu.Unlock()

		return m.stopErr
	}

	m.stopStarted = true
	m.stopped = true

	if m.state == StateRunning {
		m.state = StateStopping
	}

	m.mu.Unlock()

	err := terminateProcess(m, grace)

	m.mu.Lock()
	m.stopErr = err

	if m.state == StateRunning || m.state == StateStopping {
		m.state = StateStopped
	}

	m.mu.Unlock()

	close(m.stopDone)

	return err
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

// State returns the deterministic lifecycle state.
func (m *ManagedProcess) State() ProcessState {
	if m == nil {
		return StateRunning
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.state
}

// ExitCode returns the process exit code, or -1 while the process is
// still running (or the code is unknown).
func (m *ManagedProcess) ExitCode() int {
	if m == nil {
		return -1
	}

	select {
	case <-m.exited:
		m.mu.Lock()
		defer m.mu.Unlock()

		return m.exitCode

	default:
		return -1
	}
}

// JobBound reports whether the process is protected by the
// kernel-level kill handle (Windows job object). False means the
// supervised process-tree fallback is active — a visible, never
// silent degradation.
func (m *ManagedProcess) JobBound() bool {
	if m == nil {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.jobBound
}

// Diagnostics returns the structured supervision snapshot.
func (m *ManagedProcess) Diagnostics() ProcessDiagnostics {
	if m == nil {
		return ProcessDiagnostics{}
	}

	m.mu.Lock()
	state := m.state
	note := m.supervisionNote
	bound := m.jobBound
	code := m.exitCode
	errText := ""

	if m.err != nil {
		errText = m.err.Error()
	}

	m.mu.Unlock()

	return ProcessDiagnostics{
		Name:            m.spec.Name,
		PID:             m.pid,
		State:           state.String(),
		JobBound:        bound,
		SupervisionNote: note,
		StartedAt:       m.StartedAt,
		ExitCode:        code,
		ExitError:       errText,
		Alive:           m.Running(),
	}
}

// closeJob releases the OS-level kill handle (Windows job object).
// Safe to call any number of times: the handle is swapped under the
// mutex, and the kernel kill-on-close semantics only fire on the
// final close. On non-Windows platforms it is a no-op.
func (m *ManagedProcess) closeJob() {
	if m == nil {
		return
	}

	m.mu.Lock()
	job := m.job
	m.job = nil
	m.mu.Unlock()

	if job != nil {
		job.close()
	}
}

// PID returns the operating-system process identifier.
func (m *ManagedProcess) PID() int {
	if m == nil {
		return 0
	}

	return m.pid
}

// terminateProcess runs the shared two-phase termination flow:
// polite shutdown (platform signal where one exists) + grace wait,
// then forced termination (job kill / process-tree kill / group
// kill), then a bounded wait for reaping, then job-handle release.
// The platform layers provide politeShutdown and hardKill.
func terminateProcess(m *ManagedProcess, grace time.Duration) error {
	politeShutdown(m)

	select {
	case <-m.exited:
		m.closeJob()
		return nil

	case <-time.After(grace):
	}

	hardKill(m)

	select {
	case <-m.exited:
		m.closeJob()
		return nil

	case <-time.After(hardKillDeadline):
		// Extraordinary: the kernel kill was issued but the child
		// still shows alive. Surface it loudly — never pretend
		// cleanup succeeded.
		err := firerrors.New(firerrors.KindEnvironment,
			Subsystem, "stop",
			"process %d survived forced termination after %s",
			m.pid, hardKillDeadline)
		logging.Err("system", "stop_failed", "stop", string(firerrors.KindEnvironment),
			"pid %d did not exit after forced termination", m.pid)

		return err
	}
}

// orDiscard defaults absent writers to the sink so the child never
// blocks on full pipe buffers.
func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}

	return w
}

// launchError wraps a child-creation failure with the launch-failure
// category (distinct from readiness/exit/cancellation/environment
// failures).
func launchError(spec ProcessSpec, err error) error {
	return firerrors.Wrap(err, firerrors.KindDependencyUnavailable,
		Subsystem, "start", "launch %q", spec.Path)
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

// ExecutableName returns the platform-correct file name for a core
// executable: "v2ray" on Unix-like systems, "v2ray.exe" on Windows.
// Staging and discovery MUST both go through this helper so a binary
// staged under the managed cores directory is always found again.
func ExecutableName(name string) string {
	return executableName(name)
}

// versionProbeForms are the argument forms protocol cores accept for
// version queries. Xray supports --version; V2Ray (V2Fly v5) and
// sing-box use a "version" subcommand. Every form is tried until one
// succeeds so discovery never depends on the core family.
var versionProbeForms = [][]string{
	{"--version"},
	{"-version"},
	{"version"},
}

// queryCoreVersion asks a core for its version string. It returns an
// empty string when every probe form fails; discovery then reports the
// binary without version information rather than failing.
//
// The probe runs through concealChild: protocol cores are
// console-subsystem executables and discovery runs while the user is
// interacting with the GUI, so a visible CMD flash is a regression
// (§v0.9.2 console audit).
func queryCoreVersion(ctx context.Context, path string) string {
	for _, args := range versionProbeForms {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)

		cmd := exec.CommandContext(probeCtx, path, args...)
		concealChild(cmd)

		out, err := cmd.CombinedOutput()
		cancel()

		if err != nil {
			continue
		}

		line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		if strings.TrimSpace(line) != "" {
			return line
		}
	}

	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}
