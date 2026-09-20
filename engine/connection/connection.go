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
//	WaitingForReady → Connected → Verifying → ConnectedVerified →
//	Disconnecting → Disconnected
//
//	Failure from any state → ConnectionFailed
//
// v0.9.8.3: "connected" means the local route is established (the
// proxy listener accepts connections) — it is NOT Internet
// connectivity. Final success requires an end-to-end verification
// through the tunnel; only then does the session reach
// ConnectedVerified. A configured user port preference is respected
// for every attempt.
package connection

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/engine/metrics"

	"github.com/Parsaetak/FreeIran/engine/provider"
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

	// StateConnected: the local route is established — the core
	// process runs and its proxy listener accepts connections. This is
	// readiness evidence, not Internet connectivity (v0.9.8.3).
	StateConnected State = "connected"

	// StateVerifying: an end-to-end Internet verification is running
	// through the established route (v0.9.8.3).
	StateVerifying State = "verifying"

	// StateConnectedVerified: the route carried real traffic to the
	// verification target — usable Internet connectivity (v0.9.8.3).
	StateConnectedVerified State = "connected_verified"

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
		s == StateConnected || s == StateVerifying ||
		s == StateConnectedVerified
}

// ConnectedLike reports whether the state describes an established
// local route (with or without completed Internet verification).
func (s State) ConnectedLike() bool {
	return s == StateConnected || s == StateVerifying ||
		s == StateConnectedVerified
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
//
// v0.9.7 metric semantics: CoreReadyMS is the LOCAL core startup→ready
// duration; PingMedianMS / URLTotalMS come from an actual tunnel
// verification (end-to-end). A ready listener is NOT Internet
// connectivity — the two value families never substitute for each
// other. LatencyMS mirrors the best known end-to-end measurement and
// is empty until a verification (or tunnel probe) produced one.
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

	// CorePID is the operating-system process id of the active core
	// (0 while no core session runs). It exists so lifecycle tests and
	// diagnostics can PROVE process teardown after a session ends
	// (v0.9.8.6): observing state=disconnected/connection_failed must
	// mean the supervised process is gone, not merely unmanaged.
	CorePID int `json:"core_pid,omitempty"`

	// v0.9.7 separated measurements.
	CoreReadyMS  int64  `json:"core_ready_ms,omitempty"`
	PingMedianMS int64  `json:"ping_median_ms,omitempty"`
	URLTotalMS   int64  `json:"url_total_ms,omitempty"`
	Verification string `json:"verification,omitempty"` // none|usable|failed|degraded

	// v0.9.8.5 stability evidence (§2.3): when the session was last
	// verified usable, and how many consecutive stability rechecks
	// have failed since (0 while healthy).
	VerifiedAt     int64 `json:"verified_at,omitempty"`
	VerifyFailures int   `json:"verify_failures,omitempty"`
}

// VerifyPolicy controls the mandatory end-to-end verification that
// gates final connection success (v0.9.8.3). The zero value means
// "verification required with default timeout/target" — readiness is
// never success; only Skip disables verification (tests).
type VerifyPolicy struct {
	// Skip disables the end-to-end verification gate. Production
	// wiring never sets this; it exists for tests with no network.
	Skip bool

	// Timeout bounds one verification round-trip (default 12s).
	Timeout time.Duration

	// Target overrides the verification URL (default: the standard
	// 204 endpoint).
	Target string

	// Targets is the bounded multi-target verification set
	// (v0.9.8.5). Empty selects DefaultVerifyTargets (three
	// independent operators, quorum = majority). Target (singular)
	// takes precedence when set.
	Targets []string
}

// WithDefaults applies the verification defaults.
func (p VerifyPolicy) WithDefaults() VerifyPolicy {
	resolved := p

	if resolved.Timeout <= 0 {
		resolved.Timeout = DefaultVerifyTimeout
	}

	return resolved
}

// verifyOptions renders the policy as one verification request.
func (p VerifyPolicy) verifyOptions() VerifyOptions {
	opts := VerifyOptions{Timeout: p.Timeout}

	if p.Target != "" {
		opts.URL = p.Target
	} else {
		opts.Targets = p.Targets
	}

	return opts
}

// Options configure the connection manager.
type Options struct {
	// Registry resolves backends.
	Registry *core.Registry

	// Metrics receives protocol-core counters (nil = uncounted).
	Metrics *metrics.Registry

	// Verify gates final success on a real Internet verification
	// through the established route. Zero value = required with
	// defaults (v0.9.8.3: readiness is never success).
	Verify VerifyPolicy

	// StartupTimeout bounds core startup (default: core default).
	StartupTimeout time.Duration

	// GracePeriod bounds polite process shutdown.
	GracePeriod time.Duration

	// MonitorInterval paces health checks while connected (0 = 10s).
	MonitorInterval time.Duration

	// VerifyInterval paces the post-connection stability rechecks
	// (0 = 30s): a ConnectedVerified session is periodically
	// re-verified so degradation is DETECTED, not assumed (v0.9.8.5
	// §2.3). One failed recheck never tears a healthy session down.
	VerifyInterval time.Duration

	// VerifyGrace is the shorter recheck delay after one failed
	// recheck (0 = 8s) — the "grace / recheck" window that filters
	// single-probe flapping before any degradation decision.
	VerifyGrace time.Duration

	// VerifyFailureThreshold is the number of CONSECUTIVE failed
	// rechecks (0 = 3) after which the session is declared degraded
	// and torn down, handing control to the bounded recovery loop.
	VerifyFailureThreshold int

	// LaunchEnv carries additional environment variables for core
	// processes launched by this manager (asset directories, test
	// failure injection). Never derived from untrusted data.
	LaunchEnv []string
}

// Manager owns the single active connection session.
type Manager struct {
	opts Options

	mu       sync.Mutex
	state    State
	instance *core.Instance

	// v0.9.8.3: user-selected local inbound ports (0 = automatic).
	socksPortPref int
	httpPortPref  int

	// lastPref remembers the selection preferences of the last Connect
	// so Reconnect keeps the user's preferred backend (v0.9.8.3 fix).
	lastPref core.Preferences

	// provider holds the first-class provider session (Tor/Psiphon)
	// when the active route is provider-based (§11). Exactly one of
	// instance/provider is non-nil while connected.
	provider *providerSession

	// lastProvider remembers the provider of the last provider-based
	// session so Reconnect can re-establish it (mirrors m.cfg, which
	// persists across Disconnect for the same purpose).
	lastProvider provider.Provider
	cfg          *config.Config
	coreName     string
	coreVersion  string
	lastError    string
	attempts     []Attempt
	startedAt    time.Time
	latencyMS    int64
	port         int

	// corePID is the process id of the active core session (0 when
	// none); mirrored into Snapshot so teardown can be PROVEN from
	// outside the package (v0.9.8.6).
	corePID int

	// coreReadyMS is the LOCAL core startup→ready duration of the
	// active session (v0.9.7: never reported as network latency).
	coreReadyMS int64

	// v0.9.6: post-connect verification state (see verify.go).
	verifiedAt time.Time
	lastVerify VerifyResult

	// v0.9.8.5 stability evidence (§2.3): consecutive failed
	// stability rechecks and the time of the next scheduled recheck.
	// A single failed recheck marks the session degraded but never
	// tears it down; the threshold does.
	verifyFailures int
	nextVerifyAt   time.Time

	// generation is the session epoch (v0.9.8.6): it increments on
	// every session boundary — Connect accepting a new session,
	// Disconnect, Shutdown, a provider session starting and the
	// stability-teardown ending one. Every ASYNCHRONOUS verification
	// (monitor recheck, connect-time probe, VerifyConnected) captures
	// the generation when it starts and may apply its result only
	// while that generation is still current. A stale result is
	// discarded: it can never mutate — or poison — a newer session.
	generation uint64

	monitorCancel context.CancelFunc
	// monitorDone is closed when the monitor goroutine exits;
	// stopMonitor JOINS it so returning from Disconnect/Shutdown means
	// no monitor work (including an in-flight teardown started by the
	// stability path) is still running (v0.9.8.6).
	monitorDone chan struct{}
	shutdown    bool

	// v0.9.8.7 event-driven UI synchronization: subscribers receive the
	// authoritative Snapshot after every real state mutation. notifyCh
	// (buffered 1) collapses bursts; publishLoop dispatches the newest
	// snapshot outside all locks; pubStopped serializes the close
	// under m.mu (see publish.go / stateChanged).
	subMu       sync.Mutex
	subscribers map[int]func(Snapshot)
	subSeq      int
	notifyCh    chan struct{}
	pubDone     chan struct{}
	pubStopped  bool
}

// New creates a connection manager bound to a core registry.
func New(opts Options) *Manager {
	if opts.Registry == nil {
		opts.Registry = core.NewRegistry(nil)
	}

	if opts.MonitorInterval <= 0 {
		opts.MonitorInterval = 10 * time.Second
	}

	// v0.9.8.5 stability pacing (§2.3).
	if opts.VerifyInterval <= 0 {
		opts.VerifyInterval = 30 * time.Second
	}

	if opts.VerifyGrace <= 0 {
		opts.VerifyGrace = 8 * time.Second
	}

	if opts.VerifyFailureThreshold <= 0 {
		opts.VerifyFailureThreshold = 3
	}

	opts.Verify = opts.Verify.WithDefaults()

	m := &Manager{opts: opts, state: StateDisconnected}

	// v0.9.8.7: the snapshot dispatch goroutine starts with the
	// manager; Shutdown joins it (stopPublisher) after the terminal
	// state transitions.
	m.notifyCh = make(chan struct{}, 1)
	m.pubDone = make(chan struct{})
	go m.publishLoop()

	return m
}

// SetLocalPorts stores the user-selected local inbound ports for
// subsequent connections (0 = automatic allocation). The values are
// validated by the caller (SettingsService) and re-checked before
// every launch.
func (m *Manager) SetLocalPorts(socks, http int) {
	if m == nil {
		return
	}

	m.mu.Lock()
	m.socksPortPref = socks
	m.httpPortPref = http
	m.mu.Unlock()
}

// LocalPorts returns the active port preference.
func (m *Manager) LocalPorts() (socks int, http int) {
	if m == nil {
		return 0, 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.socksPortPref, m.httpPortPref
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

	// Session boundary (v0.9.8.6): a new session epoch starts here.
	// Every async result captured before this point becomes stale; a
	// Disconnect/Reconnect during this Connect cannot have its later
	// state mutations clobber the newer session state.
	m.generation++

	gen := m.generation

	m.cfg = &cfg
	m.state = StateSelecting
	m.attempts = nil
	m.lastError = ""
	m.coreReadyMS = 0
	m.lastPref = pref
	m.corePID = 0

	m.stateChanged()

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
		snapshot, err := m.attempt(ctx, candidate, cfg, registry, index > 0, gen)
		if err == nil {
			return snapshot, nil
		}

		lastErr = err

		// Context cancelled: stop retrying immediately.
		if ctx.Err() != nil {
			break
		}

		// The session was disconnected/superseded while an attempt was
		// running: its remaining candidates belong to a dead session.
		if m.currentGeneration() != gen {
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

			snapshot, err = m.attempt(ctx, candidate, cfg, registry, index > 0, gen)
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

	return m.failAt(gen, lastErr)
}

// currentGeneration returns the session epoch (caller-friendly
// accessor, lock-internal use only via mu).
func (m *Manager) currentGeneration() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.generation
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

// attempt runs one backend through prepare → start → ready. The
// session generation gates every state mutation after an awaitable
// step (process start, readiness, verification): a session that was
// disconnected or superseded mid-attempt gets its instance closed
// deterministically and the attempt fails honestly — it can never
// resurrect state or leak an unowned process (v0.9.8.6).
func (m *Manager) attempt(
	ctx context.Context,
	backend core.Core,
	cfg config.Config,
	registry *core.Registry,
	isFallback bool,
	gen uint64,
) (Snapshot, error) {
	started := time.Now()

	// --- Preparing -------------------------------------------------
	if isFallback && m.opts.Metrics != nil {
		m.opts.Metrics.AddCoreFallback()
	}

	m.mu.Lock()

	// Generation gate: a stale attempt (its session ended before
	// the attempt started) must not touch the state machine a
	// newer session owns.
	if m.generation != gen {
		m.mu.Unlock()

		return Snapshot{}, firerrors.New(firerrors.KindCancelled,
			Subsystem, "connect", "session ended before %s attempt prepared", backend.Name())
	}

	m.state = StatePreparing
	m.stateChanged()
	m.mu.Unlock()

	if err := backend.Validate(ctx, cfg); err != nil {
		m.recordAttemptAt(gen, backend.Name(), false, err, started)

		if m.opts.Metrics != nil {
			m.opts.Metrics.AddCoreStart(false)
		}

		return m.staleOrWrap(gen, err, firerrors.KindInvalidInput,
			"%s validation failed", backend.Name())
	}

	opts := core.RuntimeOptions{
		BinaryPath:     registry.BinaryPath(backend.Name()),
		StartupTimeout: m.opts.StartupTimeout,
		GracePeriod:    m.opts.GracePeriod,
		Env:            m.opts.LaunchEnv,
	}

	// Port stability across reconnects: reuse the session port when
	// it was released cleanly; otherwise allocate fresh. A user-selected
	// port (v0.9.8.3) overrides both.
	opts.LocalPort = m.nextPort()

	socks, http := m.userPorts()

	if socks > 0 {
		opts.LocalPort = socks
	}

	if http > 0 {
		opts.HTTPPort = http
	}

	// Bindability pre-check for user-selected ports: fail fast with a
	// useful error instead of a core-level bind crash. The core's own
	// bind remains the final authority (check-then-use races resolve
	// as a normal failed attempt).
	if err := m.checkPortBindability(opts.LocalPort, opts.HTTPPort); err != nil {
		m.recordAttemptAt(gen, backend.Name(), false, err, started)

		return Snapshot{}, err
	}

	// --- StartingCore ----------------------------------------------
	m.mu.Lock()

	if m.generation != gen {
		m.mu.Unlock()

		return Snapshot{}, firerrors.New(firerrors.KindCancelled,
			Subsystem, "connect", "session ended before %s launch", backend.Name())
	}

	m.state = StateStartingCore
	m.stateChanged()
	m.mu.Unlock()

	instance, err := backend.Start(ctx, cfg, opts)
	if err != nil {
		m.recordAttemptAt(gen, backend.Name(), false, err, started)

		if m.opts.Metrics != nil {
			m.opts.Metrics.AddCoreStart(false)
		}

		return m.staleOrWrap(gen, err, firerrors.KindDependencyUnavailable,
			"%s failed to start", backend.Name())
	}

	// --- WaitingForReady -------------------------------------------
	m.mu.Lock()

	// Generation gate: a Disconnect/Shutdown that ran while the process
	// was being spawned means this instance is unowned — it must never
	// be published into the manager (that would leak a process nobody
	// will close).
	if m.generation != gen {
		m.mu.Unlock()

		_ = instance.Close()

		return Snapshot{}, firerrors.New(firerrors.KindCancelled,
			Subsystem, "connect", "session ended during %s startup", backend.Name())
	}

	m.state = StateWaitingForReady
	m.instance = instance
	m.stateChanged()
	m.mu.Unlock()

	if err := instance.WaitReady(ctx); err != nil {
		m.recordAttemptAt(gen, backend.Name(), false, err, started)

		if m.opts.Metrics != nil {
			m.opts.Metrics.AddCoreStart(false)
		}

		// Deterministic cleanup of the failed attempt.
		_ = instance.Close()

		m.mu.Lock()

		// Generation gate: only the owning session may clear the slot —
		// a newer session may already own m.instance.
		if m.generation == gen && m.instance == instance {
			m.instance = nil
		}

		m.mu.Unlock()

		return m.staleOrWrap(gen, err, firerrors.KindDependencyUnavailable,
			"%s did not become ready", backend.Name())
	}

	// --- Connected -------------------------------------------------
	if m.opts.Metrics != nil {
		m.opts.Metrics.AddCoreStart(true)
		m.opts.Metrics.ObserveCoreStartup(time.Since(started))
	}

	coreReadyMS := time.Since(started).Milliseconds()

	// Warm the local listener probe (readiness evidence only; the
	// loopback latency is deliberately NOT stored as network latency).
	_ = instance.Health(ctx)

	m.mu.Lock()

	// Generation gate: the session must still own the state machine
	// before its readiness evidence is published.
	if m.generation != gen {
		m.mu.Unlock()

		_ = instance.Close()

		return Snapshot{}, firerrors.New(firerrors.KindCancelled,
			Subsystem, "connect", "session ended while %s became ready", backend.Name())
	}

	m.instance = instance
	m.coreName = backend.Name()
	m.coreVersion = registry.Version(backend.Name())
	m.state = StateConnected
	m.startedAt = time.Now().UTC()
	m.corePID = instance.PID()
	// v0.9.7: the loopback listener probe is stored as the local
	// readiness fallback ONLY — it is not network latency and the
	// end-to-end fields stay empty until a real verification runs.
	m.coreReadyMS = coreReadyMS
	m.port = opts.LocalPort
	m.stateChanged()
	m.mu.Unlock()

	// --- Verifying (v0.9.8.3): readiness is NOT success -------------
	// The listener accepting loopback connections proves the core
	// runs; only an end-to-end request through the tunnel proves
	// usable Internet. A failed verification fails the attempt and
	// lets the next candidate run. v0.9.8.5: the verification itself
	// probes a bounded multi-target set with a quorum rule.
	if !m.opts.Verify.Skip {
		m.mu.Lock()
		m.state = StateVerifying
		m.stateChanged()
		m.mu.Unlock()

		result := VerifyTunnel(ctx, instance.Endpoint(), m.opts.Verify.verifyOptions())

		m.mu.Lock()

		// Generation gate (v0.9.8.6): the verification ran while the
		// session was current, but a Disconnect/Reconnect may have
		// completed in the meantime (this probe is awaitable and
		// bounded only by its own timeout). A stale result — success OR
		// failure — must never mutate the newer session's state.
		if m.generation != gen {
			m.mu.Unlock()

			// The instance was closed by the session-boundary path
			// (Disconnect/Shutdown); Close is idempotent, so this is a
			// deterministic no-op reaping in case it was not.
			_ = instance.Close()

			return Snapshot{}, firerrors.New(firerrors.KindCancelled,
				Subsystem, "connect",
				"session ended during %s verification (result discarded)", backend.Name())
		}

		m.lastVerify = result

		if result.OK {
			m.verifiedAt = time.Now().UTC()
			m.verifyFailures = 0
			m.nextVerifyAt = m.verifiedAt.Add(m.opts.VerifyInterval)
		}

		m.mu.Unlock()

		if !result.OK {
			// Deterministic cleanup of the unusable route.
			_ = instance.Close()

			m.mu.Lock()

			// Generation gate (see above).
			if m.generation == gen {
				m.instance = nil
				m.state = StateSelecting // the next candidate may run
				m.stateChanged()
			}

			m.mu.Unlock()

			err := firerrors.New(firerrors.KindDependencyUnavailable,
				Subsystem, "connect", "%s started but Internet verification "+
					"failed (%s); route not usable", backend.Name(), result.Describe())

			m.recordAttemptAt(gen, backend.Name(), false, err, started)

			return Snapshot{}, err
		}
	}

	// --- Verified (or tests-only skip) ------------------------------
	if m.opts.Metrics != nil {
		m.opts.Metrics.AddCoreStart(true)
		m.opts.Metrics.ObserveCoreStartup(time.Since(started))
	}

	m.mu.Lock()

	if m.generation != gen {
		// The session ended between verification success and the final
		// transition (re-checked under the lock; the instance is already
		// closed by the boundary path).
		m.mu.Unlock()

		return Snapshot{}, firerrors.New(firerrors.KindCancelled,
			Subsystem, "connect", "session ended before %s became verified", backend.Name())
	}

	if m.opts.Verify.Skip {
		m.state = StateConnected
	} else {
		m.state = StateConnectedVerified
	}

	m.stateChanged()

	m.mu.Unlock()

	m.recordAttemptAt(gen, backend.Name(), true, nil, started)

	m.startMonitor()

	return m.Snapshot(), nil
}

// userPorts returns the user-selected local inbound preferences
// (0 = automatic for each).
func (m *Manager) userPorts() (socks, http int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.socksPortPref, m.httpPortPref
}

// staleOrWrap wraps an attempt failure for the ORIGINAL caller, but
// classifies it as cancelled when the session ended while the
// attempt ran (v0.9.8.6) — a superseded session's failure is not a
// connectivity verdict about anything.
func (m *Manager) staleOrWrap(gen uint64, err error, kind firerrors.Kind, format string, args ...any) (Snapshot, error) {
	if m.currentGeneration() != gen {
		return Snapshot{}, firerrors.New(firerrors.KindCancelled,
			Subsystem, "connect", "session ended during attempt (%v)", err)
	}

	return Snapshot{}, firerrors.Wrap(err, kind,
		Subsystem, "connect", format, args...)
}

// checkPortBindability probes whether the requested local inbound
// ports can actually be bound on the IPv4 loopback before a core
// launch consumes them. A port the caller explicitly asked for must
// fail loudly (naming the port and the conflict) instead of surfacing
// later as a cryptic core bind error.
func (m *Manager) checkPortBindability(socks, http int) error {
	for _, port := range []int{socks, http} {
		if port <= 0 {
			continue // 0 = automatic ephemeral allocation
		}

		if port < 1024 || port > 65535 {
			return firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "connect", "local proxy port %d out of range (1024-65535)", port)
		}

		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return firerrors.New(firerrors.KindDependencyUnavailable,
				Subsystem, "connect", "local proxy port %d is already in use; "+
					"choose another port or free it (bind check: %v)", port, err)
		}

		_ = ln.Close()
	}

	return nil
}

// Disconnect tears the session down: stop the core first (Windows
// file-lock discipline), then release resources, then clear state.
// Provider sessions (§11) stop through their own deterministic
// lifecycle — no orphan processes either way.
//
// v0.9.8.6 determinism contract: returning from Disconnect means the
// session epoch has ended (generation bumped BEFORE any teardown),
// the monitor goroutine has JOINED (an in-flight stability recheck
// and its process teardown completed or was discarded), and the
// instance was closed — the supervised process is gone or the error
// was recorded in lastError. Nothing keeps mutating the manager
// afterwards.
func (m *Manager) Disconnect() Snapshot {
	if m == nil {
		return Snapshot{}
	}

	// Session boundary FIRST: any in-flight verification (monitor
	// recheck, connect-time probe) captured an older generation and
	// will therefore DISCARD its result instead of applying it to the
	// post-disconnect state.
	m.mu.Lock()
	m.generation++
	m.mu.Unlock()

	// Join the monitor: cancel its context and WAIT for the goroutine
	// to exit. The goroutine's current iteration (including a
	// stability teardown that is closing the instance) finishes before
	// Disconnect proceeds — an in-flight Close is never orphaned.
	m.stopMonitor()

	m.mu.Lock()

	instance := m.instance
	m.instance = nil
	m.state = StateDisconnecting
	m.corePID = 0

	// v0.9.8.5: clear the stability evidence with the session.
	// v0.9.8.6: the verification verdicts are session evidence too —
	// a disconnected session reports "none", never a stale
	// "degraded"/"failed" label produced by a recheck that the
	// generation guard discarded.
	m.verifyFailures = 0
	m.nextVerifyAt = time.Time{}
	m.verifiedAt = time.Time{}
	m.lastVerify = VerifyResult{}

	m.stateChanged()

	m.mu.Unlock()

	// Provider sessions stop through the provider lifecycle.
	m.stopProviderSession()

	if instance != nil {
		if err := instance.Close(); err != nil {
			m.mu.Lock()
			m.lastError = fmt.Sprintf("disconnect: %v", err)
			m.stateChanged()
			m.mu.Unlock()
		}
	}

	m.mu.Lock()
	m.state = StateDisconnected
	m.stateChanged()
	m.mu.Unlock()

	return m.Snapshot()
}

// Reconnect re-establishes the last configuration — or the last
// provider session when the previous route was provider-based (§11).
func (m *Manager) Reconnect(ctx context.Context) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, firerrors.New(firerrors.KindFatal,
			Subsystem, "reconnect", "manager is nil")
	}

	m.mu.Lock()
	cfg := m.cfg
	provSession := m.provider
	m.mu.Unlock()

	if provSession != nil && cfg == nil {
		// Provider session: reconnect through the same provider.
		prov := provSession.prov

		m.Disconnect()

		return m.ConnectProvider(ctx, prov, VerifyOptions{})
	}

	if provSession == nil && cfg == nil {
		// After a Disconnect the active-session fields are cleared;
		// the last ROUTE is remembered for reconnect (config or
		// provider).
		m.mu.Lock()
		lastProv := m.lastProvider
		m.mu.Unlock()

		if lastProv != nil {
			m.Disconnect()

			return m.ConnectProvider(ctx, lastProv, VerifyOptions{})
		}
	}

	if cfg == nil {
		return m.Snapshot(), firerrors.New(firerrors.KindConfiguration,
			Subsystem, "reconnect", "no previous configuration to reconnect")
	}

	m.Disconnect()

	// v0.9.8.3: keep the user's preferred backend across reconnects.
	pref := m.lastPref
	pref.AllowFallback = true

	return m.Connect(ctx, *cfg, pref)
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
		CorePID:     m.corePID,
	}

	if m.cfg != nil {
		snapshot.ConfigID = m.cfg.ID
		snapshot.ConfigName = m.cfg.Name
		snapshot.ConfigDisplay = m.cfg.DisplayURL()
	}

	if m.provider != nil {
		// Provider-backed session (§11): the same verification
		// semantics, reported through the same Snapshot surface.
		snapshot.Endpoint = m.provider.endpoint
		snapshot.CoreReadyMS = m.coreReadyMS
		snapshot.applyVerification(m)

		if !m.startedAt.IsZero() {
			snapshot.StartedAt = m.startedAt.UnixMilli()
		}

		if m.cfg == nil {
			snapshot.ConfigDisplay = m.coreName + " (provider session)"
		}
	}

	if m.instance != nil {
		snapshot.Endpoint = m.instance.Endpoint()
		snapshot.CoreReadyMS = m.coreReadyMS

		// LatencyMS reflects the best known END-TO-END measurement
		// (tunnel verification), never the loopback probe.
		snapshot.applyVerification(m)

		if !m.startedAt.IsZero() {
			snapshot.StartedAt = m.startedAt.UnixMilli()
		}
	}

	return snapshot
}

// applyVerification renders the verification/stability evidence of
// the active session onto the snapshot (caller holds the lock).
// v0.9.8.5: a verified session whose latest stability recheck failed
// reports "degraded" — honestly, without tearing itself down over
// one transient failure (§2.3).
func (s *Snapshot) applyVerification(m *Manager) {
	if m.verifiedAt.IsZero() {
		s.Verification = "none"

		return
	}

	s.VerifiedAt = m.verifiedAt.UnixMilli()
	s.VerifyFailures = m.verifyFailures

	if m.lastVerify.OK {
		s.Verification = "usable"
		s.LatencyMS = m.lastVerify.TunnelProbeMS
		s.PingMedianMS = m.lastVerify.TunnelProbeMS
		s.URLTotalMS = m.lastVerify.Metrics.TotalMS

		return
	}

	if m.state == StateConnectedVerified {
		s.Verification = "degraded"
	} else {
		s.Verification = "failed"
	}
}

// VerifyConnected verifies USABLE connectivity through the ACTIVE
// session's tunnel (v0.9.6 §12: a ready listener proves the local
// core, not the path). The verification result is recorded and
// returned to the caller; a failed verification does NOT tear the
// session down - that decision belongs to the recovery policy, which
// uses the failure class.
func (m *Manager) VerifyConnected(ctx context.Context, opts VerifyOptions) (VerifyResult, error) {
	if m == nil {
		return VerifyResult{}, firerrors.New(firerrors.KindFatal,
			Subsystem, "verify", "manager is nil")
	}

	m.mu.Lock()
	instance := m.instance
	providerSession := m.provider
	state := m.state
	gen := m.generation
	m.mu.Unlock()

	if (instance == nil && providerSession == nil) || !state.ConnectedLike() {
		return VerifyResult{}, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "verify",
			"no active session to verify (state %s)", state)
	}

	// One endpoint, one verification model: core sessions and provider
	// sessions share the same multi-target gate (v0.9.8.5 §2.3).
	endpoint := ""

	if instance != nil {
		endpoint = instance.Endpoint()
	} else if providerSession != nil {
		endpoint = providerSession.endpoint
	}

	result := VerifyTunnel(ctx, endpoint, opts)

	m.mu.Lock()

	// Generation gate (v0.9.8.6): a session boundary (disconnect /
	// reconnect / shutdown) completed while this probe was running —
	// the result describes a route that no longer belongs to the
	// manager's current session, so it is returned to the caller but
	// NOT applied (a stale failure must never poison the newer
	// session's stability evidence).
	if m.generation != gen {
		m.mu.Unlock()

		return result, nil
	}

	m.lastVerify = result

	if result.OK {
		m.verifiedAt = time.Now().UTC()

		// A manual successful verification is fresh evidence: the
		// stability counter resets (the monitor schedules its own next
		// recheck from here).
		m.verifyFailures = 0
		m.nextVerifyAt = m.verifiedAt.Add(m.opts.VerifyInterval)
	}

	m.stateChanged()

	m.mu.Unlock()

	return result, nil
}

// LastVerification returns the most recent verification result of the
// active session (zero value when never verified).
func (m *Manager) LastVerification() VerifyResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lastVerify
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

	// v0.9.7: a loopback probe latency is readiness evidence only —
	// it never replaces the startup duration or the verified
	// end-to-end measurement.
	_ = report

	m.mu.Unlock()

	return report
}

// Shutdown disconnects and permanently disables the manager
// (application shutdown path). Like Disconnect it is deterministic:
// the session epoch ends and the monitor joins before returning.
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

	// v0.9.8.7: join the snapshot dispatch goroutine AFTER the terminal
	// state transitions so the final disconnected snapshot is still
	// delivered. No listener callback can run after this returns.
	m.stopPublisher()
}

// startMonitor launches the health-monitor loop bounded by ctx.
//
// v0.9.8.5 (§2.3): the loop carries TWO evidence axes —
//
//	process health    (core/provider alive — failures are immediate)
//	stability health  (periodic multi-target re-verification through
//	                   the active session — failures are cumulative)
//
// A single failed stability recheck NEVER tears a healthy session
// down: the failure marks the session degraded and schedules a fast
// grace recheck; only CONSECUTIVE failures beyond the threshold
// transition the session to ConnectionFailed, handing control to
// the bounded recovery loop (fresh-test → re-rank → reconnect).
//
// v0.9.8.6: the loop captures the session generation together with
// the instance on every tick; every mutation and the crash transition
// are generation-gated so a tick observing a superseded session can
// never touch the newer one. The goroutine closes monitorDone on
// exit — stopMonitor JOINS on it.
func (m *Manager) startMonitor() {
	m.stopMonitor()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	m.mu.Lock()
	m.monitorCancel = cancel
	m.monitorDone = done
	gen := m.generation
	m.mu.Unlock()

	interval := m.opts.MonitorInterval

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				m.mu.Lock()
				instance := m.instance
				provSession := m.provider
				state := m.state
				tickGen := m.generation
				m.mu.Unlock()

				if (instance == nil && provSession == nil) || !state.ConnectedLike() {
					return
				}

				// The session this tick observed was superseded
				// (a disconnect/reconnect completed between
				// scheduling and running): stop touching the
				// manager entirely.
				if tickGen != gen {
					return
				}

				// Provider sessions (§11): the provider's own health
				// (process alive + measured endpoint) drives the crash
				// transition — same failure semantics as cores.
				if provSession != nil {
					health := provSession.prov.Health(ctx)
					if !health.ProcessAlive {
						if m.opts.Metrics != nil {
							m.opts.Metrics.AddCoreCrash()
						}

						m.mu.Lock()

						if m.generation == tickGen && m.state.ConnectedLike() {
							m.state = StateConnectionFailed
							m.lastError = "provider process exited unexpectedly"
							m.provider = nil
							m.stateChanged()
						}

						m.mu.Unlock()

						// Deterministic cleanup of the dead
						// provider's resources (bounded; the
						// process is already gone).
						stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
						_ = provSession.prov.Stop(stopCtx)
						stopCancel()

						return
					}

					// Stability re-verification for provider
					// sessions: the same multi-target gate,
					// the same thresholds (§2.3).
					m.runStabilityRecheck(ctx, provSession.endpoint, tickGen)

					continue
				}

				report := instance.Health(ctx)
				if !report.ProcessAlive {
					// Core crashed under our feet: transition.
					if m.opts.Metrics != nil {
						m.opts.Metrics.AddCoreCrash()
					}

					m.mu.Lock()

					if m.generation == tickGen && m.state.ConnectedLike() {
						m.state = StateConnectionFailed
						m.lastError = "core process exited unexpectedly"
						m.instance = nil
						m.corePID = 0
						m.stateChanged()
					}

					m.mu.Unlock()

					// Deterministic cleanup of the crashed
					// instance's owned files (process dead →
					// handles releasable; Close is safe on an
					// exited process). Dropping the instance
					// without Close — the v0.9.8.5 behaviour —
					// leaked its temporary runtime directory.
					_ = instance.Close()

					return
				}

				m.mu.Lock()
				// v0.9.7: loopback probe latency is NOT
				// network latency — the verified end-to-end
				// measurement stays authoritative.
				_ = report
				m.mu.Unlock()

				// Stability re-verification for core sessions.
				m.runStabilityRecheck(ctx, instance.Endpoint(), tickGen)
			}
		}
	}()
}

// runStabilityRecheck performs ONE bounded multi-target recheck of
// the active session when the pacing schedule says so, and applies
// the documented degradation policy:
//
//	connected_verified → recheck failed → (grace) recheck again →
//	still failing × threshold → teardown → connection_failed → recovery
//
// A success at ANY point resets the failure evidence and restores
// the normal recheck pacing — there is no permanent "healthy"
// label without measured evidence.
//
// v0.9.8.6 determinism (the Windows teardown contract): the teardown
// completes BEFORE the ConnectionFailed state becomes observable, and
// a session superseded while the recheck ran never mutates the newer
// session (generation gate). If the teardown cannot PROVE termination
// (instance.Close error), the error is preserved in lastError —
// evidence, never silence.
func (m *Manager) runStabilityRecheck(ctx context.Context, endpoint string, gen uint64) {
	if endpoint == "" {
		return
	}

	m.mu.Lock()

	if m.state != StateConnectedVerified || m.opts.Verify.Skip {
		m.mu.Unlock()

		return
	}

	if m.generation != gen {
		// The session was superseded between the monitor tick
		// and this recheck: a stale recheck never runs at all.
		m.mu.Unlock()

		return
	}

	if m.nextVerifyAt.IsZero() || time.Now().Before(m.nextVerifyAt) {
		m.mu.Unlock()

		return
	}

	// Reserve the slot so one slow recheck is never started
	// twice (the next attempt is scheduled from the outcome).
	m.nextVerifyAt = time.Now().Add(m.opts.VerifyInterval)

	m.mu.Unlock()

	// Bounded, cancellable probe OUTSIDE the lock.
	probeCtx, cancel := context.WithTimeout(ctx, m.opts.Verify.Timeout)
	defer cancel()

	result := VerifyTunnel(probeCtx, endpoint, m.opts.Verify.verifyOptions())

	m.mu.Lock()

	// Generation gate (v0.9.8.6): a disconnect/reconnect/shutdown
	// completed while the probe ran. The result — success OR
	// failure — describes a route that no longer exists; discarding
	// it is the only correct action (a stale failure must never
	// poison a newer session, and a stale success must never
	// "heal" one either).
	if m.generation != gen {
		m.mu.Unlock()

		return
	}

	m.lastVerify = result

	if result.OK {
		m.verifyFailures = 0
		m.verifiedAt = time.Now().UTC()
		m.nextVerifyAt = m.verifiedAt.Add(m.opts.VerifyInterval)

		m.stateChanged()

		m.mu.Unlock()

		return
	}

	m.verifyFailures++

	// Grace / recheck: one (or a few) failed recheck keeps the
	// session alive but schedules a fast follow-up probe — no
	// single-probe flapping tears down a healthy session.
	if m.verifyFailures < m.opts.VerifyFailureThreshold {
		m.nextVerifyAt = time.Now().Add(m.opts.VerifyGrace)

		m.stateChanged()

		m.mu.Unlock()

		return
	}

	// Threshold reached: the verified session has no measured
	// evidence of usable Internet anymore — tear it down and let
	// the bounded recovery loop take over with FRESH evidence.
	//
	// The session ENDS here (v0.9.8.6): grab the resources, mark
	// Disconnecting and bump the generation so late observers can
	// distinguish a live teardown from a finished failure, then
	// run the bounded teardown BEFORE the terminal state becomes
	// observable.
	degradedMsg := fmt.Sprintf("connection degraded: %d consecutive failed verification rechecks (%s)",
		m.verifyFailures, result.Describe())

	instance := m.instance
	provSession := m.provider

	m.instance = nil
	m.provider = nil
	m.corePID = 0
	m.verifyFailures = 0
	m.state = StateDisconnecting
	m.lastError = degradedMsg

	// The epoch ends with the session: verifications captured by
	// the old generation are stale from this point on.
	m.generation++

	m.stateChanged()

	m.mu.Unlock()

	// Deterministic teardown OUTSIDE the lock and BEFORE the
	// terminal state becomes observable (Windows file-lock
	// discipline: stop the process first). Completing this block
	// with a nil teardown error means: child exited, descendants
	// reaped, owned runtime files removed. A non-nil error means
	// termination could NOT be proven — it is preserved below.
	var teardownErr error

	if instance != nil {
		teardownErr = instance.Close()
	}

	if provSession != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)

		if err := provSession.prov.Stop(stopCtx); err != nil && teardownErr == nil {
			teardownErr = err
		}

		stopCancel()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-check under the lock: a Disconnect/Shutdown that ran while
	// the teardown was in progress owns the terminal state now —
	// this goroutine must not overwrite it.
	if m.state == StateDisconnecting {
		m.state = StateConnectionFailed
	}

	// Teardown evidence: a process that could not be proven dead
	// is reported, never silently dropped.
	if teardownErr != nil {
		m.lastError = fmt.Sprintf("%s; teardown error: %v", degradedMsg, teardownErr)
	}

	m.stateChanged()
}

// stopMonitor cancels the monitor loop and JOINS the goroutine.
// v0.9.8.6: cancelling alone is not synchronization — the goroutine
// may be mid-recheck or mid-teardown. Waiting for monitorDone makes
// "the monitor has stopped" a proved fact: any in-flight verification
// finished (and, if a stability teardown had started, the process
// teardown completed) before this returns. The wait is bounded by
// construction: the probe honours ctx cancellation, and the teardown
// path is bounded by the grace period plus the hard-kill deadline.
func (m *Manager) stopMonitor() {
	m.mu.Lock()
	cancel := m.monitorCancel
	done := m.monitorDone
	m.monitorCancel = nil
	m.monitorDone = nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	if done != nil {
		<-done
	}
}

// fail records the failure and returns the failed snapshot.
func (m *Manager) fail(err error) (Snapshot, error) {
	return m.failAt(0, err)
}

// failAt records the failure and returns the failed snapshot — but
// only while the session generation gen is still current (v0.9.8.6).
// A stale attempt (its session was disconnected or superseded while
// it ran) must not clobber the state machine a newer session owns:
// the error is returned to the ORIGINAL caller while the manager
// state is left untouched. gen == 0 records unconditionally (legacy
// internal callers whose sessions cannot be superseded).
func (m *Manager) failAt(gen uint64, err error) (Snapshot, error) {
	m.mu.Lock()

	if gen == 0 || m.generation == gen {
		m.state = StateConnectionFailed
		m.lastError = err.Error()
		m.instance = nil
		m.corePID = 0
		m.stateChanged()
	}

	m.mu.Unlock()

	return m.Snapshot(), err
}

// recordAttempt appends one attempt record.
func (m *Manager) recordAttempt(backend string, ok bool, err error, started time.Time) {
	m.recordAttemptAt(0, backend, ok, err, started)
}

// recordAttemptAt appends one attempt record for the session that
// owned generation gen. A stale attempt (its session ended before the
// record was written) is skipped: it must not pollute a newer
// session's attempt history. gen == 0 records unconditionally.
func (m *Manager) recordAttemptAt(gen uint64, backend string, ok bool, err error, started time.Time) {
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

	if gen == 0 || m.generation == gen {
		m.attempts = append(m.attempts, attempt)
		m.stateChanged()
	}

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
	socks := m.socksPortPref
	m.mu.Unlock()

	// A user-selected port wins on every attempt (the bindability
	// check surfaces conflicts as a clear per-attempt failure).
	if socks > 0 {
		return socks
	}

	if port > 0 {
		return port
	}

	// Let the launcher allocate an ephemeral port.
	return 0
}
