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

	return &Manager{opts: opts, state: StateDisconnected}
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

	m.cfg = &cfg
	m.state = StateSelecting
	m.attempts = nil
	m.lastError = ""
	m.coreReadyMS = 0
	m.lastPref = pref

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

		if m.opts.Metrics != nil {
			m.opts.Metrics.AddCoreStart(false)
		}

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
		m.recordAttempt(backend.Name(), false, err, started)

		return Snapshot{}, err
	}

	// --- StartingCore ----------------------------------------------
	m.mu.Lock()
	m.state = StateStartingCore
	m.mu.Unlock()

	instance, err := backend.Start(ctx, cfg, opts)
	if err != nil {
		m.recordAttempt(backend.Name(), false, err, started)

		if m.opts.Metrics != nil {
			m.opts.Metrics.AddCoreStart(false)
		}

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

		if m.opts.Metrics != nil {
			m.opts.Metrics.AddCoreStart(false)
		}

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

	coreReadyMS := time.Since(started).Milliseconds()

	// Warm the local listener probe (readiness evidence only; the
	// loopback latency is deliberately NOT stored as network latency).
	_ = instance.Health(ctx)

	m.mu.Lock()
	m.instance = instance
	m.coreName = backend.Name()
	m.coreVersion = registry.Version(backend.Name())
	m.state = StateConnected
	m.startedAt = time.Now().UTC()
	// v0.9.7: the loopback listener probe is stored as the local
	// readiness fallback ONLY — it is not network latency and the
	// end-to-end fields stay empty until a real verification runs.
	m.coreReadyMS = coreReadyMS
	m.port = opts.LocalPort
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
		m.mu.Unlock()

		result := VerifyTunnel(ctx, instance.Endpoint(), m.opts.Verify.verifyOptions())

		m.mu.Lock()
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
			m.instance = nil
			m.state = StateSelecting // the next candidate may run
			m.mu.Unlock()

			err := firerrors.New(firerrors.KindDependencyUnavailable,
				Subsystem, "connect", "%s started but Internet verification "+
					"failed (%s); route not usable", backend.Name(), result.Describe())

			m.recordAttempt(backend.Name(), false, err, started)

			return Snapshot{}, err
		}
	}

	// --- Verified (or tests-only skip) ------------------------------
	if m.opts.Metrics != nil {
		m.opts.Metrics.AddCoreStart(true)
		m.opts.Metrics.ObserveCoreStartup(time.Since(started))
	}

	m.mu.Lock()

	if m.opts.Verify.Skip {
		m.state = StateConnected
	} else {
		m.state = StateConnectedVerified
	}

	m.mu.Unlock()

	m.recordAttempt(backend.Name(), true, nil, started)

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
func (m *Manager) Disconnect() Snapshot {
	if m == nil {
		return Snapshot{}
	}

	m.stopMonitor()

	m.mu.Lock()

	instance := m.instance
	m.instance = nil
	m.state = StateDisconnecting

	// v0.9.8.5: clear the stability evidence with the session.
	m.verifyFailures = 0
	m.nextVerifyAt = time.Time{}

	m.mu.Unlock()

	// Provider sessions stop through the provider lifecycle.
	m.stopProviderSession()

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
	m.lastVerify = result

	if result.OK {
		m.verifiedAt = time.Now().UTC()

		// A manual successful verification is fresh evidence: the
		// stability counter resets (the monitor schedules its own next
		// recheck from here).
		m.verifyFailures = 0
		m.nextVerifyAt = m.verifiedAt.Add(m.opts.VerifyInterval)
	}

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
				provSession := m.provider
				state := m.state
				m.mu.Unlock()

				if (instance == nil && provSession == nil) || !state.ConnectedLike() {
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

						if m.state.ConnectedLike() {
							m.state = StateConnectionFailed
							m.lastError = "provider process exited unexpectedly"
							m.provider = nil
						}

						m.mu.Unlock()

						return
					}

					// Stability re-verification for provider
					// sessions: the same multi-target gate,
					// the same thresholds (§2.3).
					m.runStabilityRecheck(ctx, provSession.endpoint)

					continue
				}

				report := instance.Health(ctx)
				if !report.ProcessAlive {
					// Core crashed under our feet: transition.
					if m.opts.Metrics != nil {
						m.opts.Metrics.AddCoreCrash()
					}

					m.mu.Lock()

					if m.state.ConnectedLike() {
						m.state = StateConnectionFailed
						m.lastError = "core process exited unexpectedly"
						m.instance = nil
					}

					m.mu.Unlock()

					return
				}

				m.mu.Lock()
				// v0.9.7: loopback probe latency is NOT
				// network latency — the verified end-to-end
				// measurement stays authoritative.
				_ = report
				m.mu.Unlock()

				// Stability re-verification for core sessions.
				m.runStabilityRecheck(ctx, instance.Endpoint())
			}
		}
	}()
}

// runStabilityRecheck performs ONE bounded multi-target recheck of
// the active session when the pacing schedule says so, and applies
// the documented degradation policy:
//
//	connected_verified → recheck failed → (grace) recheck again →
//	still failing × threshold → connection_failed → recovery
//
// A success at ANY point resets the failure evidence and restores
// the normal recheck pacing — there is no permanent "healthy"
// label without measured evidence.
func (m *Manager) runStabilityRecheck(ctx context.Context, endpoint string) {
	if endpoint == "" {
		return
	}

	m.mu.Lock()

	if m.state != StateConnectedVerified || m.opts.Verify.Skip {
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
	defer m.mu.Unlock()

	m.lastVerify = result

	if result.OK {
		m.verifyFailures = 0
		m.verifiedAt = time.Now().UTC()
		m.nextVerifyAt = m.verifiedAt.Add(m.opts.VerifyInterval)

		return
	}

	m.verifyFailures++

	// Grace / recheck: one (or a few) failed recheck keeps the
	// session alive but schedules a fast follow-up probe — no
	// single-probe flapping tears down a healthy session.
	if m.verifyFailures < m.opts.VerifyFailureThreshold {
		m.nextVerifyAt = time.Now().Add(m.opts.VerifyGrace)

		return
	}

	// Threshold reached: the verified session has no measured
	// evidence of usable Internet anymore — tear it down and let
	// the bounded recovery loop take over with FRESH evidence.
	m.lastError = fmt.Sprintf("connection degraded: %d consecutive failed verification rechecks (%s)",
		m.verifyFailures, result.Describe())
	m.state = StateConnectionFailed
	m.verifyFailures = 0

	instance := m.instance
	provSession := m.provider

	m.instance = nil
	m.provider = nil

	m.mu.Unlock()

	// Deterministic cleanup outside the lock (Windows file-lock
	// discipline: stop the process first).
	if instance != nil {
		_ = instance.Close()
	}

	if provSession != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = provSession.prov.Stop(stopCtx)
		stopCancel()
	}

	m.mu.Lock()
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
