// Package connection implements the high-level connection service:
// one active session, explicit lifecycle states, deterministic
// backend selection with bounded fallback, health monitoring and
// cleanup.
//
// The state machine is the single source of truth — there are no
// ambiguous isRunning/isConnected booleans that can contradict each
// other:
//
//	Disconnected → Selecting → Preparing → StartingCore →
//	WaitingForReady → Connected → Disconnecting → Disconnected
//
//	Failure from any state → ConnectionFailed
package connection

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/engine/metrics"
)

// Subsystem identifies the connection layer in structured errors.
const Subsystem = "connection"

// State is the explicit connection lifecycle state.
type State string

const (
	// StateDisconnected: no session.
	StateDisconnected State = "disconnected"

	// StateSelecting: resolving a backend for the configuration.
	StateSelecting State = "selecting"

	// StatePreparing: validating and generating the runtime config.
	StatePreparing State = "preparing"

	// StateStartingCore: spawning the protocol-core process.
	StateStartingCore State = "starting_core"

	// StateWaitingForReady: polling the local listener.
	StateWaitingForReady State = "waiting_for_ready"

	// StateConnected: tunnel established (listener verified).
	StateConnected State = "connected"

	// StateDisconnecting: stopping the core and cleaning up.
	StateDisconnecting State = "disconnecting"

	// StateConnectionFailed: the last attempt failed; no session.
	StateConnectionFailed State = "connection_failed"
)

// Terminal reports whether no further transition is pending.
func (s State) Terminal() bool {
	return s == StateDisconnected || s == StateConnectionFailed
}

// Active reports whether a session exists right now.
func (s State) Active() bool {
	return s == StateSelecting || s == StatePreparing ||
		s == StateStartingCore || s == StateWaitingForReady ||
		s == StateConnected
}

// Attempt records one backend attempt for observability (§41).
type Attempt struct {
	Backend  string    `json:"backend"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	Duration int64     `json:"duration_ms"`
	At       time.Time `json:"at"`
}

// Snapshot is the immutable state view surfaced to the UI (§16):
// connection state, backend identity, configuration identity,
// latency and the attempt history — all credential-free.
type Snapshot struct {
	State         State     `json:"state"`
	Core          string    `json:"core,omitempty"`
	CoreVersion   string    `json:"core_version,omitempty"`
	ConfigID      string    `json:"config_id,omitempty"`
	ConfigName    string    `json:"config_name,omitempty"`
	ConfigDisplay string    `json:"config_display,omitempty"`
	Endpoint      string    `json:"endpoint,omitempty"`
	LatencyMS     int64     `json:"latency_ms,omitempty"`
	StartedAt     int64     `json:"started_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	Attempts      []Attempt `json:"attempts,omitempty"`
	FallbacksUsed int       `json:"fallbacks_used,omitempty"`
}

// Options configure the connection manager.
type Options struct {
	// Registry resolves backends.
	Registry *core.Registry

	// Metrics receives protocol-core counters (nil = uncounted).
	Metrics *metrics.Registry

	// StartupTimeout bounds core startup (default: core default).
	StartupTimeout time.Duration

	// GracePeriod bounds polite process shutdown.
	GracePeriod time.Duration

	// MonitorInterval paces health checks while connected (0 = 10s).
	MonitorInterval time.Duration

	// LaunchEnv carries additional environment variables for core
	// processes launched by this manager (asset directories, test
	// failure injection). Never derived from untrusted data.
	LaunchEnv []string
}

// Manager owns the single active connection session.
type Manager struct {
	opts Options

	mu          sync.Mutex
	state       State
	instance    *core.Instance
	cfg         *config.Config
	coreName    string
	coreVersion string
	lastError   string
	attempts    []Attempt
	startedAt   time.Time
	latencyMS   int64
	port        int

	monitorCancel context.CancelFunc
	shutdown      bool
}

// New creates a connection manager bound to a core registry.
func New(opts Options) *Manager {
	if opts.Registry == nil {
		opts.Registry = core.NewRegistry(nil)
	}

	if opts.MonitorInterval <= 0 {
		opts.MonitorInterval = 10 * time.Second
	}

	return &Manager{opts: opts, state: StateDisconnected}
}

// Connect establishes the tunnel for one configuration.
//
// The flow is deterministic and observable:
//
//	config → capability resolution → candidate list →
//	prepare (validate + generate) → start core → wait ready →
//	connected; bounded fallback on failure, every attempt recorded.
func (m *Manager) Connect(
	ctx context.Context,
	cfg config.Config,
	pref core.Preferences,
) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, firerrors.New(firerrors.KindFatal,
			Subsystem, "connect", "manager is nil")
	}

	m.mu.Lock()

	if m.shutdown {
		m.mu.Unlock()

		return Snapshot{}, firerrors.New(firerrors.KindFatal,
			Subsystem, "connect", "manager is shut down")
	}

	if m.state.Active() {
		snapshot := m.snapshotLocked()

		m.mu.Unlock()

		return snapshot, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "connect",
			"a connection is already active (state %s)", m.state)
	}

	cfg.Normalize()

	cfg.SetID()

	m.cfg = &cfg
	m.state = StateSelecting
	m.attempts = nil
	m.lastError = ""
	m.latencyMS = 0

	m.mu.Unlock()

	// --- Selecting -------------------------------------------------
	registry := m.opts.Registry

	selection, err := registry.Select(cfg, pref)
	if err != nil {
		return m.fail(err)
	}

	m.mu.Lock()
	m.coreName = selection.Core.Name()
	m.coreVersion = registry.Version(selection.Core.Name())
	m.mu.Unlock()

	// Candidate list: preferred selection first, then declared
	// fallbacks, bounded by MaxAttempts.
	candidates := []core.Core{selection.Core}

	if pref.AllowFallback {
		for _, name := range selection.Fallbacks {
			if len(candidates) >= pref.WithDefaults().MaxAttempts {
				break
			}

			if fallback, ok := registry.Get(name); ok {
				candidates = append(candidates, fallback)
			}
		}
	}

	var lastErr error

	for index, candidate := range candidates {
		snapshot, err := m.attempt(ctx, candidate, cfg, registry, index > 0)
		if err == nil {
			return snapshot, nil
		}

		lastErr = err

		// Context cancelled: stop retrying immediately.
		if ctx.Err() != nil {
			break
		}

		// Startup crashes get exactly one same-backend retry with a
		// FRESH port: the ephemeral reserve window can lose a port to
		// a concurrent process, which is a transient condition, not a
		// backend incompatibility.
		if m.state == StateConnectionFailed && isStartupCrash(lastErr) {
			m.mu.Lock()
			m.port = 0 // force fresh allocation
			m.mu.Unlock()

			snapshot, err = m.attempt(ctx, candidate, cfg, registry, index > 0)
			if err == nil {
				return snapshot, nil
			}

			lastErr = err
		}
	}

	if lastErr == nil {
		lastErr = firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "connect", "no backend attempt succeeded")
	}

	return m.fail(lastErr)
}

// isStartupCrash reports whether an attempt failed because the core
// process died during startup (bind-steal / transient spawn issues)
// rather than a capability or validation mismatch.
func isStartupCrash(err error) bool {
	if err == nil {
		return false
	}

	return strings.Contains(err.Error(), "exited during startup") ||
		strings.Contains(err.Error(), "did not become ready")
}

// attempt runs one backend through prepare → start → ready.
func (m *Manager) attempt(
	ctx context.Context,
	backend core.Core,
	cfg config.Config,
	registry *core.Registry,
	isFallback bool,
) (Snapshot, error) {
	started := time.Now()

	// --- Preparing -------------------------------------------------
	if isFallback && m.opts.Metrics != nil {
		m.opts.Metrics.AddCoreFallback()
	}

	m.mu.Lock()
	m.state = StatePreparing
	m.mu.Unlock()

	if err := backend.Validate(ctx, cfg); err != nil {
		m.recordAttempt(backend.Name(), false, err, started)
		m.opts.Metrics.AddCoreStart(false)

		return Snapshot{}, firerrors.Wrap(err, firerrors.KindInvalidInput,
			Subsystem, "connect", "%s validation failed", backend.Name())
	}

	opts := core.RuntimeOptions{
		BinaryPath:     registry.BinaryPath(backend.Name()),
		StartupTimeout: m.opts.StartupTimeout,
		GracePeriod:    m.opts.GracePeriod,
		Env:            m.opts.LaunchEnv,
	}

	// Port stability across reconnects: reuse the session port when
	// it was released cleanly; otherwise allocate fresh.
	opts.LocalPort = m.nextPort()

	// --- StartingCore ----------------------------------------------
	m.mu.Lock()
	m.state = StateStartingCore
	m.mu.Unlock()

	instance, err := backend.Start(ctx, cfg, opts)
	if err != nil {
		m.recordAttempt(backend.Name(), false, err, started)
		m.opts.Metrics.AddCoreStart(false)

		return Snapshot{}, firerrors.Wrap(err, firerrors.KindDependencyUnavailable,
			Subsystem, "connect", "%s failed to start", backend.Name())
	}

	// --- WaitingForReady -------------------------------------------
	m.mu.Lock()
	m.state = StateWaitingForReady
	m.instance = instance
	m.mu.Unlock()

	if err := instance.WaitReady(ctx); err != nil {
		m.recordAttempt(backend.Name(), false, err, started)
		m.opts.Metrics.AddCoreStart(false)

		// Deterministic cleanup of the failed attempt.
		_ = instance.Close()

		m.mu.Lock()
		m.instance = nil
		m.mu.Unlock()

		return Snapshot{}, firerrors.Wrap(err, firerrors.KindDependencyUnavailable,
			Subsystem, "connect", "%s did not become ready", backend.Name())
	}

	// --- Connected -------------------------------------------------
	if m.opts.Metrics != nil {
		m.opts.Metrics.AddCoreStart(true)
		m.opts.Metrics.ObserveCoreStartup(time.Since(started))
	}

	health := instance.Health(ctx)

	m.mu.Lock()
	m.instance = instance
	m.coreName = backend.Name()
	m.coreVersion = registry.Version(backend.Name())
	m.state = StateConnected
	m.startedAt = time.Now().UTC()
	m.latencyMS = health.LatencyMS
	m.port = opts.LocalPort
	m.mu.Unlock()

	m.recordAttempt(backend.Name(), true, nil, started)

	m.startMonitor()

	return m.Snapshot(), nil
}

// Disconnect tears the session down: stop the core first (Windows
// file-lock discipline), then release resources, then clear state.
func (m *Manager) Disconnect() Snapshot {
	if m == nil {
		return Snapshot{}
	}

	m.stopMonitor()

	m.mu.Lock()

	instance := m.instance
	m.instance = nil
	m.state = StateDisconnecting

	m.mu.Unlock()

	if instance != nil {
		if err := instance.Close(); err != nil {
			m.mu.Lock()
			m.lastError = fmt.Sprintf("disconnect: %v", err)
			m.mu.Unlock()
		}
	}

	m.mu.Lock()
	m.state = StateDisconnected
	m.mu.Unlock()

	return m.Snapshot()
}

// Reconnect re-establishes the last configuration.
func (m *Manager) Reconnect(ctx context.Context) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, firerrors.New(firerrors.KindFatal,
			Subsystem, "reconnect", "manager is nil")
	}

	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	if cfg == nil {
		return m.Snapshot(), firerrors.New(firerrors.KindConfiguration,
			Subsystem, "reconnect", "no previous configuration to reconnect")
	}

	m.Disconnect()

	return m.Connect(ctx, *cfg, core.Preferences{AllowFallback: true})
}

// Snapshot returns the credential-free state view.
func (m *Manager) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{State: StateDisconnected}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.snapshotLocked()
}

// snapshotLocked renders the snapshot (caller holds the lock).
func (m *Manager) snapshotLocked() Snapshot {
	snapshot := Snapshot{
		State:       m.state,
		Core:        m.coreName,
		CoreVersion: m.coreVersion,
		LastError:   m.lastError,
		Attempts:    append([]Attempt(nil), m.attempts...),
	}

	if m.cfg != nil {
		snapshot.ConfigID = m.cfg.ID
		snapshot.ConfigName = m.cfg.Name
		snapshot.ConfigDisplay = m.cfg.DisplayURL()
	}

	if m.instance != nil {
		snapshot.Endpoint = m.instance.Endpoint()
		snapshot.LatencyMS = m.latencyMS

		if !m.startedAt.IsZero() {
			snapshot.StartedAt = m.startedAt.UnixMilli()
		}
	}

	return snapshot
}

// State returns the current lifecycle state.
func (m *Manager) State() State {
	if m == nil {
		return StateDisconnected
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.state
}

// Health measures the active session (process + listener axes).
func (m *Manager) Health(ctx context.Context) core.HealthReport {
	m.mu.Lock()
	instance := m.instance
	m.mu.Unlock()

	if instance == nil {
		return core.HealthReport{}
	}

	report := instance.Health(ctx)

	m.mu.Lock()

	if report.LatencyMS > 0 {
		m.latencyMS = report.LatencyMS
	}

	m.mu.Unlock()

	return report
}

// Shutdown disconnects and permanently disables the manager
// (application shutdown path).
func (m *Manager) Shutdown() {
	if m == nil {
		return
	}

	m.mu.Lock()
	m.shutdown = true
	m.mu.Unlock()

	m.Disconnect()

	m.mu.Lock()
	m.cfg = nil
	m.attempts = nil
	m.mu.Unlock()
}

// startMonitor launches the health-monitor loop bounded by ctx.
func (m *Manager) startMonitor() {
	m.stopMonitor()

	ctx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	m.monitorCancel = cancel
	m.mu.Unlock()

	interval := m.opts.MonitorInterval

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				m.mu.Lock()
				instance := m.instance
				state := m.state
				m.mu.Unlock()

				if instance == nil || state != StateConnected {
					return
				}

				report := instance.Health(ctx)
				if !report.ProcessAlive {
					// Core crashed under our feet: transition.
					if m.opts.Metrics != nil {
						m.opts.Metrics.AddCoreCrash()
					}

					m.mu.Lock()

					if m.state == StateConnected {
						m.state = StateConnectionFailed
						m.lastError = "core process exited unexpectedly"
						m.instance = nil
					}

					m.mu.Unlock()

					return
				}

				m.mu.Lock()
				m.latencyMS = report.LatencyMS
				m.mu.Unlock()
			}
		}
	}()
}

// stopMonitor cancels the monitor loop and waits for it to observe
// the cancellation (bounded by the next tick).
func (m *Manager) stopMonitor() {
	m.mu.Lock()
	cancel := m.monitorCancel
	m.monitorCancel = nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// fail records the failure and returns the failed snapshot.
func (m *Manager) fail(err error) (Snapshot, error) {
	m.mu.Lock()
	m.state = StateConnectionFailed
	m.lastError = err.Error()
	m.instance = nil
	m.mu.Unlock()

	return m.Snapshot(), err
}

// recordAttempt appends one attempt record.
func (m *Manager) recordAttempt(backend string, ok bool, err error, started time.Time) {
	attempt := Attempt{
		Backend:  backend,
		OK:       ok,
		Duration: time.Since(started).Milliseconds(),
		At:       time.Now().UTC(),
	}

	if err != nil {
		attempt.Error = core.RedactLogText(err.Error(), m.secrets())
	}

	m.mu.Lock()
	m.attempts = append(m.attempts, attempt)
	m.mu.Unlock()
}

// secrets collects the active configuration's credential fields.
func (m *Manager) secrets() []string {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	if cfg == nil {
		return nil
	}

	return cfg.SecretFields()
}

// nextPort resolves the local port for an attempt: the session port
// when still valid, a fresh allocation otherwise.
func (m *Manager) nextPort() int {
	m.mu.Lock()
	port := m.port
	m.mu.Unlock()

	if port > 0 {
		return port
	}

	// Let the launcher allocate an ephemeral port.
	return 0
}
