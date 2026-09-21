// provider.go implements provider sessions on the EXISTING
// connection engine (§11): Quick Connect may select a configuration,
// Tor or Psiphon, but every path uses the same high-level lifecycle —
// select → start provider/core → wait ready → establish route →
// verify actual Internet → connected → monitor → recover.
//
// There is no duplicate process manager (providers supervise their
// own single managed process through system.ManagedProcess), no
// duplicate recovery engine (the same monitor loop and failure states
// apply) and no bypassing verification (the same VerifyTunnel gate
// runs through the provider's local SOCKS endpoint before the session
// is called Connected).
package connection

import (
	"context"
	"fmt"
	"time"

	"github.com/Parsaetak/FreeIran/engine/provider"
)

// providerSession is the provider-backed session surface the manager
// drives: the provider owns its process; the connection manager owns
// the session state machine, verification and monitoring.
type providerSession struct {
	prov     provider.Provider
	endpoint string
}

// ConnectProvider establishes the tunnel through a first-class
// provider (Tor, Psiphon): the provider is started (its own start
// already waits for readiness), its local SOCKS endpoint becomes the
// route, and the SAME Internet verification gate runs before the
// session is Connected. A failed verification or start leaves the
// manager in ConnectionFailed with the provider deterministically
// stopped.
func (m *Manager) ConnectProvider(
	ctx context.Context,
	prov provider.Provider,
	opts VerifyOptions,
) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, fmt.Errorf("connection manager is nil")
	}

	if prov == nil {
		return m.fail(fmt.Errorf("provider is nil"))
	}

	m.mu.Lock()

	if m.shutdown {
		m.mu.Unlock()

		return m.fail(fmt.Errorf("connection manager is shut down"))
	}

	m.mu.Unlock()

	// --- SELECT ------------------------------------------------------
	m.mu.Lock()

	// Session boundary (v0.9.8.6): a new provider session epoch.
	//
	// v0.9.10: this is a full SESSION REPLACEMENT — the manager allows
	// a provider session to take over from a running core-based session
	// (and vice versa through Connect, which requires the previous
	// session to have ended). The previous session's resources are
	// captured here and torn down deterministically BELOW the lock:
	// the pre-0.9.10 code dropped m.instance/m.provider without closing
	// them, so the replaced session's core or provider process kept
	// running forever (an orphan the user could only see in Task
	// Manager). The replacement teardown happens BEFORE the provider
	// starts, so a failed provider start never leaves two half-owned
	// processes behind.
	m.generation++

	gen := m.generation

	m.beginSessionLocked()

	previousInstance := m.instance
	previousProvider := m.provider

	m.state = StateSelecting
	m.cfg = nil
	m.instance = nil
	m.provider = nil
	m.corePID = 0
	m.lastProvider = prov
	m.attempts = nil
	m.lastError = ""
	m.stateChanged()
	m.mu.Unlock()

	// Deterministic replacement teardown (outside the manager lock):
	// the superseded session's processes stop exactly as Disconnect
	// would stop them.
	if previousInstance != nil {
		_ = previousInstance.Close()
	}

	if previousProvider != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = previousProvider.prov.Stop(stopCtx)
		stopCancel()
	}

	// --- START PROVIDER (includes wait-ready/bootstrap) ---------------
	m.mu.Lock()
	m.state = StatePreparing
	m.stateChanged()

	// v0.9.10: the provider process is launched on the SESSION runtime
	// context — its lifetime belongs to the active session, not to the
	// caller's bounded operation context (mirrors the core launch
	// rule; the provider's own bootstrap/negotiate timeouts bound the
	// readiness wait).
	sessCtx := m.sessionCtx
	m.mu.Unlock()

	if sessCtx == nil {
		sessCtx = context.Background() // defensive (see attempt)
	}

	started := time.Now()

	startErr := prov.Start(sessCtx)

	coreReady := time.Since(started)

	if startErr != nil {
		// Deterministic cleanup: never leave a half-started provider.
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = prov.Stop(stopCtx)
		cancel()

		// v0.9.9: the start is awaitable — a session boundary may have
		// completed while it ran; the stale failure must not clobber a
		// newer session's state (failAt is generation-gated).
		return m.failAt(gen, fmt.Errorf("provider %s failed to start: %w", prov.Name(), startErr))
	}

	// --- ESTABLISH ROUTE ----------------------------------------------
	endpoints := prov.Endpoints()

	var endpoint string

	for _, ep := range endpoints {
		if ep.Network == "socks5" {
			endpoint = ep.Addr()

			break
		}
	}

	if endpoint == "" {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = prov.Stop(stopCtx)
		cancel()

		// v0.9.9: generation-gated (see the start-failure note above).
		return m.failAt(gen, fmt.Errorf("provider %s exposes no local SOCKS endpoint", prov.Name()))
	}

	info := prov.Info()

	m.mu.Lock()

	// Generation gate (v0.9.8.6): the session was ended
	// (Disconnect/Shutdown) while the provider was starting — stop it
	// and fail honestly instead of publishing a session nobody owns.
	if m.generation != gen {
		m.mu.Unlock()

		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = prov.Stop(stopCtx)
		cancel()

		return m.failAt(gen, fmt.Errorf("session ended while provider %s was starting", prov.Name()))
	}

	m.provider = &providerSession{prov: prov, endpoint: endpoint}
	m.coreName = info.Name
	m.coreVersion = info.Version
	m.coreReadyMS = coreReady.Milliseconds()
	m.startedAt = time.Now().UTC()
	m.stateChanged()
	m.mu.Unlock()

	// --- VERIFY ACTUAL INTERNET (no bypassing) -------------------------
	m.mu.Lock()
	m.state = StateWaitingForReady
	m.stateChanged()
	m.mu.Unlock()

	opts = opts.normalize()

	result := VerifyTunnel(ctx, endpoint, opts)

	m.mu.Lock()

	// Generation gate (v0.9.8.6): a session boundary completed while
	// the provider verification was in flight. A stale result — success
	// OR failure — is discarded: it must never resurrect a torn-down
	// session nor poison a newer one.
	if m.generation != gen {
		m.mu.Unlock()

		// The provider belongs to a dead session: stop it
		// deterministically (Stop is idempotent).
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = prov.Stop(stopCtx)
		cancel()

		return m.failAt(gen, fmt.Errorf(
			"session ended during provider %s verification (result discarded)", prov.Name()))
	}

	m.lastVerify = result

	if result.OK {
		m.verifiedAt = time.Now().UTC()

		// v0.9.8.5 (§2.3): provider sessions join the same
		// stability recheck schedule as core sessions.
		m.verifyFailures = 0
		m.nextVerifyAt = m.verifiedAt.Add(m.opts.VerifyInterval)
	}

	m.stateChanged()

	m.mu.Unlock()

	if !result.OK {
		// A provider that cannot reach the Internet is disconnected
		// deterministically (verification failure, honestly reported).
		//
		// v0.9.9: every mutation below is generation-gated — a session
		// boundary that completed while the verification ran owns the
		// state machine now; the stale session must only reap its own
		// provider, never mutate the newer session.
		m.mu.Lock()
		provSession := m.provider
		stale := m.generation != gen
		m.mu.Unlock()

		if provSession != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = prov.Stop(stopCtx)
			cancel()
		}

		m.mu.Lock()
		if m.generation == gen {
			m.provider = nil
			m.stateChanged()
		}
		m.mu.Unlock()

		if stale {
			return m.failAt(gen, fmt.Errorf(
				"session ended during provider %s verification (result discarded)", prov.Name()))
		}

		return m.failAt(gen, fmt.Errorf("provider %s verification failed: %s",
			prov.Name(), result.Describe()))
	}

	// --- CONNECTED_VERIFIED + MONITOR -----------------------------------
	// v0.9.8.4 contract fix: the verification gate above just proved
	// real Internet through the provider's route, so the session
	// reaches the SAME final success state as a core-based connection
	// (StateConnectedVerified). Reporting the plain StateConnected here
	// left verified Tor/Psiphon sessions looking unverified to the UI
	// (stuck on the "verifying" transitional surface).
	m.mu.Lock()

	// Generation gate (re-checked under the lock): the session must
	// still be current before the final transition and the monitor
	// start (v0.9.8.6).
	if m.generation != gen {
		m.mu.Unlock()

		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = prov.Stop(stopCtx)
		cancel()

		return m.failAt(gen, fmt.Errorf(
			"session ended before provider %s became verified", prov.Name()))
	}

	m.state = StateConnectedVerified
	m.lastError = ""
	m.stateChanged()
	m.mu.Unlock()

	m.startMonitor()

	return m.Snapshot(), nil
}

// ProviderEndpoint returns the active provider session's local
// SOCKS endpoint (empty when the session is core-based or absent).
func (m *Manager) ProviderEndpoint() string {
	if m == nil {
		return ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.provider == nil {
		return ""
	}

	return m.provider.endpoint
}

// ProviderName returns the active provider's name (empty for
// core-based sessions).
func (m *Manager) ProviderName() string {
	if m == nil {
		return ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.provider == nil {
		return ""
	}

	return m.provider.prov.Name()
}

// ActiveEndpoint returns the local SOCKS endpoint of the active
// session regardless of its kind (core instance or provider).
func (m *Manager) ActiveEndpoint() string {
	if m == nil {
		return ""
	}

	m.mu.Lock()

	instance := m.instance
	provSession := m.provider

	m.mu.Unlock()

	if instance != nil {
		return instance.Endpoint()
	}

	if provSession != nil {
		return provSession.endpoint
	}

	return ""
}

// stopProviderSession stops the provider-backed session (used by
// Disconnect and failure paths).
func (m *Manager) stopProviderSession() {
	m.mu.Lock()
	provSession := m.provider
	m.provider = nil
	m.mu.Unlock()

	if provSession == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = provSession.prov.Stop(ctx)
}
