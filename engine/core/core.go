// Package core defines FreeIran's protocol-core execution boundary: the
// backend abstraction, the capability model, backend registry,
// deterministic backend selection, and the supervised process/instance
// lifecycle shared by every backend adapter.
//
// A Core implementation (xray, v2ray, sing-box, ...) translates one
// normalized config.Config into a backend-specific runtime
// configuration and launches the backend binary through the shared
// process manager. Protocol decisions never spread beyond this package
// and its backend adapters: the rest of the application asks the
// registry which backend can execute a configuration and why.
package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/system"
)

// Subsystem identifies the core layer in structured errors.
const Subsystem = "core"

// Core is the execution backend for one protocol family.
//
// Implementations are stateless between calls; all runtime state lives
// in the returned Instance. Every method must be safe for concurrent
// use.
type Core interface {
	// Name returns the canonical backend identifier: "xray",
	// "v2ray", "sing-box".
	Name() string

	// Supports reports whether the backend can execute the
	// configuration's protocol/transport/security combination. It must
	// be deterministic, cheap and side-effect free — it consults the
	// backend's declared capabilities only.
	Supports(cfg config.Config) bool

	// Validate performs deep validation: the configuration is checked
	// against the backend's capability model and completeness rules.
	// It must not touch the network and must not start processes.
	Validate(ctx context.Context, cfg config.Config) error

	// BuildConfig converts a normalized configuration into the
	// backend's runtime configuration document. The document is JSON;
	// callers write it to a temporary file with secure permissions
	// through RunConfig. Generation is deterministic: identical
	// inputs produce identical bytes (verified by contract tests).
	BuildConfig(cfg config.Config, opts RuntimeOptions) (RuntimeConfig, error)

	// Start launches the backend for the configuration and returns a
	// supervised instance. Start returns only after the process has
	// been spawned successfully; readiness is observed separately
	// through Instance.WaitReady.
	Start(ctx context.Context, cfg config.Config, opts RuntimeOptions) (*Instance, error)
}

// RuntimeOptions tune one core launch. They are supplied by the
// connection manager or tester, never derived from untrusted source
// data.
type RuntimeOptions struct {
	// LocalHost is the inbound listen address (default 127.0.0.1).
	LocalHost string

	// LocalPort is the inbound SOCKS port. 0 = allocate an ephemeral
	// port. A fixed port is used by tests.
	LocalPort int

	// HTTPPort is an optional secondary HTTP inbound port (V2Ray/Xray
	// format only). 0 = disabled.
	HTTPPort int

	// BinaryPath overrides the discovered executable (tests, managed
	// runtimes). Empty = resolve through the registry/locator.
	BinaryPath string

	// StartupTimeout bounds the wait from spawn to listener-ready.
	// Zero = DefaultStartupTimeout.
	StartupTimeout time.Duration

	// GracePeriod is the polite-stop window before a forced kill.
	// Zero = DefaultGracePeriod.
	GracePeriod time.Duration

	// WorkDir overrides the temporary runtime directory (tests).
	// Empty = a fresh directory under the system temp root.
	WorkDir string

	// DisableGenCache bypasses the generated-config cache.
	DisableGenCache bool

	// Env carries additional environment variables for the core
	// process (asset directories, test failure injection). It is
	// never derived from untrusted source data.
	Env []string
}

// WithDefaults returns a copy of the options with defaults applied.
func (o RuntimeOptions) WithDefaults() RuntimeOptions {
	resolved := o

	if resolved.LocalHost == "" {
		resolved.LocalHost = "127.0.0.1"
	}

	if resolved.StartupTimeout <= 0 {
		resolved.StartupTimeout = DefaultStartupTimeout
	}

	if resolved.GracePeriod <= 0 {
		resolved.GracePeriod = DefaultGracePeriod
	}

	return resolved
}

// Timing defaults for core launches.
const (
	// DefaultStartupTimeout bounds spawn-to-listener-ready.
	DefaultStartupTimeout = 15 * time.Second

	// DefaultGracePeriod is the polite-stop window before SIGKILL.
	DefaultGracePeriod = 5 * time.Second
)

// RuntimeConfig is a backend-generated configuration document plus
// the metadata needed to run it safely.
type RuntimeConfig struct {
	// FileName is the suggested temporary file name (backend-specific
	// extension, e.g. "singbox-<fingerprint>.json").
	FileName string

	// Data is the JSON document.
	Data []byte

	// RedactedSummary is a credential-free description of the
	// generated document for logs and diagnostics.
	RedactedSummary string

	// Format is the config format identifier ("v4" for the
	// V2Ray/Xray JSON format, "sing-box" for sing-box JSON).
	Format string
}

// InstanceState is the explicit lifecycle state of one running core.
//
// Transitions:
//
//	Created → Starting → Running → Stopping → Stopped
//	Starting → StartFailed | TimedOut
//	Running → Crashed | Unhealthy
type InstanceState string

// Instance lifecycle and error states. There are no ambiguous boolean
// flags: the state machine is the single source of truth.
const (
	StateCreated     InstanceState = "created"
	StateStarting    InstanceState = "starting"
	StateRunning     InstanceState = "running"
	StateStopping    InstanceState = "stopping"
	StateStopped     InstanceState = "stopped"
	StateStartFailed InstanceState = "start_failed"
	StateCrashed     InstanceState = "crashed"
	StateUnhealthy   InstanceState = "unhealthy"
	StateTimedOut    InstanceState = "timed_out"
)

// Instance is one supervised running core process together with the
// resources it owns: the managed process, the captured (redacted) log
// tail and the temporary runtime configuration file.
//
// Close stops the process and removes every owned resource; it is
// idempotent and safe to call concurrently.
type Instance struct {
	core    Core
	cfg     config.Config
	opts    RuntimeOptions
	proc    *system.ManagedProcess
	logs    *LogBuffer
	runCfg  *RunConfig
	listen  string
	started time.Time

	mu       sync.Mutex
	state    InstanceState
	stopErr  error
	ready    chan struct{}
	readyErr error
	once     sync.Once
}

// setState records a lifecycle transition.
func (i *Instance) setState(state InstanceState) {
	i.mu.Lock()
	i.state = state
	i.mu.Unlock()
}

// State returns the current lifecycle state.
func (i *Instance) State() InstanceState {
	i.mu.Lock()
	defer i.mu.Unlock()

	return i.state
}

// CoreName returns the backend that owns this instance.
func (i *Instance) CoreName() string {
	if i == nil || i.core == nil {
		return ""
	}

	return i.core.Name()
}

// PID returns the operating-system process identifier (0 before
// spawn or after exit).
func (i *Instance) PID() int {
	if i == nil || i.proc == nil {
		return 0
	}

	return i.proc.PID()
}

// Endpoint returns the local SOCKS endpoint (host:port) once
// allocated, or "" before.
func (i *Instance) Endpoint() string {
	if i == nil {
		return ""
	}

	return i.listen
}

// Logs returns a redacted snapshot of the captured process output.
func (i *Instance) Logs() []string {
	if i == nil || i.logs == nil {
		return nil
	}

	return i.logs.Lines()
}

// StartedAt returns the spawn time.
func (i *Instance) StartedAt() time.Time {
	if i == nil {
		return time.Time{}
	}

	return i.started
}

// markReady signals successful readiness detection.
func (i *Instance) markReady(err error) {
	i.once.Do(func() {
		i.mu.Lock()
		i.readyErr = err

		if err == nil {
			i.state = StateRunning
		}

		i.mu.Unlock()
		close(i.ready)
	})
}

// WaitReady blocks until the local listener accepts connections, the
// process exits first, or the startup timeout elapses.
func (i *Instance) WaitReady(ctx context.Context) error {
	if i == nil {
		return firerrors.New(firerrors.KindFatal, Subsystem, "wait_ready",
			"nil instance")
	}

	timeout := i.opts.WithDefaults().StartupTimeout

	watchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Detect a process death while waiting for readiness.
	exited := make(chan error, 1)

	go func() {
		exited <- i.proc.Wait(watchCtx)
	}()

	select {
	case <-i.ready:
		return i.readyErr

	case err := <-exited:
		if err == context.Canceled || err == watchCtx.Err() {
			return firerrors.Wrap(err, firerrors.KindRetryable,
				Subsystem, "wait_ready", "startup cancelled")
		}

		i.setState(StateCrashed)

		tail := i.logs.RedactedTail(8)

		return firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "wait_ready",
			"core %s exited during startup (state %s, logs: %s)",
			i.CoreName(), i.State(), tail)

	case <-watchCtx.Done():
		i.setState(StateTimedOut)

		// The process is still alive but not serving: terminate it.
		_ = i.proc.Stop(i.opts.WithDefaults().GracePeriod)

		return firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "wait_ready",
			"core %s did not become ready within %s (state %s, logs: %s)",
			i.CoreName(), timeout, i.State(), i.logs.RedactedTail(8))
	}
}

// Health reports both process health (alive) and network health
// (local listener accepting connections). A living process does not
// imply a working tunnel; the two axes are measured independently.
func (i *Instance) Health(ctx context.Context) HealthReport {
	if i == nil {
		return HealthReport{}
	}

	report := HealthReport{
		ProcessAlive: i.proc != nil && i.proc.Running(),
		Core:         i.CoreName(),
		State:        i.State(),
		CheckedAt:    time.Now().UTC(),
	}

	if i.listen != "" {
		ready, latency, err := probeListener(ctx, i.listen)
		report.ListenerReady = ready
		report.LatencyMS = latency.Milliseconds()

		if err != nil {
			report.Details = err.Error()
		}
	}

	if report.ProcessAlive && !report.ListenerReady && i.State() == StateRunning {
		i.setState(StateUnhealthy)
		report.State = StateUnhealthy
	}

	return report
}

// Alive reports whether the supervised process is still running.
func (i *Instance) Alive() bool {
	if i == nil || i.proc == nil {
		return false
	}

	return i.proc.Running()
}

// Close stops the core and releases every owned resource: the
// process first (so the temporary configuration file is no longer
// open on Windows), then the file, then the runtime directory. Close
// is idempotent and concurrent-safe; every call returns the first
// recorded error.
func (i *Instance) Close() error {
	if i == nil {
		return nil
	}

	i.mu.Lock()

	if i.state == StateStopped || i.state == StateStopping {
		err := i.stopErr
		i.mu.Unlock()

		return err
	}

	i.state = StateStopping
	i.mu.Unlock()

	grace := i.opts.WithDefaults().GracePeriod

	if i.proc != nil {
		if err := i.proc.Stop(grace); err != nil {
			i.mu.Lock()
			i.stopErr = err
			i.state = StateStopped
			i.mu.Unlock()

			// The process may still hold the config file open; still
			// attempt removal (bounded retry) before returning.
			_ = i.runCfg.Cleanup()

			return err
		}
	}

	err := i.runCfg.Cleanup()

	i.mu.Lock()
	i.stopErr = err
	i.state = StateStopped
	i.mu.Unlock()

	return err
}

// Launch is the shared launch sequence used by every backend
// adapter: allocate the local port, write the temporary runtime
// configuration, spawn the process with captured output and wrap it
// in a supervised Instance.
func Launch(
	ctx context.Context,
	backend Core,
	cfg config.Config,
	opts RuntimeOptions,
	doc RuntimeConfig,
	runArgs func(configFile string, listenPort int) []string,
) (*Instance, error) {
	opts = opts.WithDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindInvalidInput,
			Subsystem, "launch", "invalid configuration")
	}

	port := opts.LocalPort

	if port == 0 {
		allocated, err := ReserveLocalPort(opts.LocalHost)
		if err != nil {
			return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "launch", "reserve local port")
		}

		port = allocated
	}

	binaryPath := opts.BinaryPath

	if binaryPath == "" {
		return nil, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "launch",
			"binary path must be resolved by the registry or overridden")
	}

	listen := fmt.Sprintf("%s:%d", opts.LocalHost, port)

	// The log buffer redacts credential material from captured core
	// output before anything is exposed through Logs().
	logs := NewLogBuffer(cfg)

	runCfg, err := NewRunConfig(opts.WorkDir, backend.Name())
	if err != nil {
		return nil, err
	}

	if err := runCfg.Write(doc); err != nil {
		_ = runCfg.Cleanup()

		return nil, err
	}

	proc, err := system.Start(ctx, system.ProcessSpec{
		Name:    backend.Name(),
		Path:    binaryPath,
		Args:    runArgs(runCfg.Path(), port),
		Env:     opts.Env,
		WorkDir: runCfg.Dir(),
		Stdout:  logs,
		Stderr:  logs,
	})
	if err != nil {
		// Deterministic cleanup after failed startup.
		_ = runCfg.Cleanup()

		return nil, err
	}

	instance := &Instance{
		core:    backend,
		cfg:     cfg,
		opts:    opts,
		proc:    proc,
		logs:    logs,
		runCfg:  runCfg,
		listen:  listen,
		started: time.Now().UTC(),
		ready:   make(chan struct{}),
		state:   StateStarting,
	}

	// Observe readiness in the background; WaitReady consumers get
	// the result through the ready channel.
	go func() {
		readyErr := waitForListener(ctx, instance.listen, opts.StartupTimeout, proc)
		instance.markReady(readyErr)
	}()

	return instance, nil
}
