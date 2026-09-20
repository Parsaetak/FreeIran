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
	m.generation++

	gen := m.generation

	m.state = StateSelecting
	m.cfg = nil
	m.provider = nil
	m.corePID = 0
	m.lastProvider = prov
	m.attempts = nil
	m.lastError = ""
	m.stateChanged()
	m.mu.Unlock()

	// --- START PROVIDER (includes wait-ready/bootstrap) ---------------
	m.mu.Lock()
	m.state = StatePreparing
	m.stateChanged()
	m.mu.Unlock()

	started := time.Now()

	startErr := prov.Start(ctx)

	coreReady := time.Since(started)

	if startErr != nil {
		// Deterministic cleanup: never leave a half-started provider.
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = prov.Stop(stopCtx)
		cancel()

		return m.fail(fmt.Errorf("provider %s failed to start: %w", prov.Name(), startErr))
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

		return m.fail(fmt.Errorf("provider %s exposes no local SOCKS endpoint", prov.Name()))
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
		m.mu.Lock()
		provSession := m.provider
		m.mu.Unlock()

		if provSession != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = prov.Stop(stopCtx)
			cancel()
		}

		m.mu.Lock()
		m.provider = nil
		m.stateChanged()
		m.mu.Unlock()

		return m.fail(fmt.Errorf("provider %s verification failed: %s",
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
