package connection_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
)

// verify_multi_test.go pins the v0.9.8.5 verification semantics:
//
//   - the multi-target QUORUM rule (one unrelated outage cannot fail
//     a healthy route; one lucky endpoint cannot verify a dead one);
//   - the bounded TRANSIENT retry (5xx/timeouts retry exactly once;
//     deterministic 4xx objections never retry);
//   - the stability DEGRADATION model at the manager level (a single
//     failed recheck degrades but never tears down; the consecutive
//     threshold does; a recovery resets the evidence).

// ---- helpers -----------------------------------------------------------

// countingTarget serves the given status codes in order (the last one
// repeats) and counts the requests it served.
func countingTarget(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(calls.Add(1)) - 1
		if index >= len(statuses) {
			index = len(statuses) - 1
		}

		w.WriteHeader(statuses[index])
	}))

	t.Cleanup(srv.Close)

	return srv, &calls
}

// routeRelay is a SOCKS5 relay that routes each CONNECT by the
// requested destination PORT: registered ports forward to their
// upstream, unregistered ports blackhole (SOCKS success, then
// silence) — the deterministic stand-in for a dead remote path.
type routeRelay struct {
	ln      net.Listener
	mu      sync.Mutex
	routes  map[string]string
	handler func(port string, served int)
	served  int
}

func startRouteRelay(t *testing.T) *routeRelay {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	r := &routeRelay{ln: ln, routes: map[string]string{}}

	t.Cleanup(func() { _ = ln.Close() })

	go r.serve()

	return r
}

func (r *routeRelay) addr() string { return r.ln.Addr().String() }

// route forwards one destination port to an upstream.
func (r *routeRelay) route(destPort, upstream string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.routes[destPort] = upstream
}

// portOf extracts the port of a URL's listener address.
func portOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()

	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	return port
}

func (r *routeRelay) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}

		go r.handle(conn)
	}
}

func (r *routeRelay) handle(conn net.Conn) {
	defer conn.Close()

	// SOCKS5 greeting: VER NMETHODS METHODS...
	greet := make([]byte, 3)
	if _, err := io.ReadFull(conn, greet); err != nil {
		return
	}

	if greet[0] != 0x05 {
		return
	}

	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// CONNECT request: VER CMD RSV ATYP ...
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}

	if head[1] != 0x01 {
		return
	}

	dest, err := readSocksAddr(conn, head[3])
	if err != nil {
		return
	}

	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}

	_, destPort, _ := net.SplitHostPort(dest)

	r.mu.Lock()
	upstream, routed := r.routes[destPort]
	r.served++
	n := r.served
	handler := r.handler
	r.mu.Unlock()

	if handler != nil {
		handler(destPort, n)
	}

	if !routed {
		// Blackhole: hold the connection open without any data —
		// the client's HTTP request times out (timeout class).
		_, _ = io.Copy(io.Discard, conn)

		return
	}

	up, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}

	defer up.Close()

	relayStreams(conn, up)
}

// waitFor polls until cond holds (bounded), failing the test on
// timeout with the given description.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(15 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// ---- quorum ------------------------------------------------------------

// TestVerifyMultiTargetQuorumRule pins the documented quorum: with
// three targets and one of them persistently broken, the two healthy
// endpoints carry the verification (quorum = strict majority).
func TestVerifyMultiTargetQuorumRule(t *testing.T) {
	healthyA := fakeVerifyTarget(t)
	healthyB := fakeVerifyTarget(t)
	broken, _ := countingTarget(t, http.StatusInternalServerError) // 500

	relay := startRouteRelay(t)

	relay.route(portOf(t, healthyA), healthyA.Listener.Addr().String())
	relay.route(portOf(t, healthyB), healthyB.Listener.Addr().String())
	relay.route(portOf(t, broken), broken.Listener.Addr().String())

	result := connection.VerifyTunnel(context.Background(), relay.addr(), connection.VerifyOptions{
		Targets: []string{healthyA.URL, broken.URL, healthyB.URL},
		Timeout: 4 * time.Second,
		Backoff: 50 * time.Millisecond,
	})

	if !result.OK {
		t.Fatalf("quorum verification failed: %s", result.Describe())
	}

	if result.Required != 2 {
		t.Fatalf("required = %d, want majority-of-three = 2", result.Required)
	}

	if result.Succeeded != 2 {
		t.Fatalf("succeeded = %d, want exactly the two healthy targets", result.Succeeded)
	}

	if len(result.Targets) != 3 {
		t.Fatalf("target evidence rows = %d, want 3", len(result.Targets))
	}

	// The broken target must carry its own honest evidence.
	for _, row := range result.Targets {
		if row.URL == broken.URL {
			if row.OK {
				t.Fatal("the 500 target must not be OK")
			}

			if row.Status != http.StatusInternalServerError {
				t.Fatalf("broken row status = %d, want 500", row.Status)
			}

			if row.FailureClass != connection.FailureHTTP {
				t.Fatalf("broken row class = %s, want http_status", row.FailureClass)
			}
		}
	}

	// The winning metrics must come from a healthy target.
	if result.Metrics.URL == broken.URL {
		t.Fatalf("winning metrics from the broken target: %+v", result.Metrics)
	}
}

// TestVerifyMultiTargetQuorumFailure pins the other side of the
// quorum rule: one healthy endpoint among three is NOT a majority —
// verification fails honestly.
func TestVerifyMultiTargetQuorumFailure(t *testing.T) {
	healthy := fakeVerifyTarget(t)
	brokenA, _ := countingTarget(t, http.StatusInternalServerError)
	brokenB, _ := countingTarget(t, http.StatusBadGateway)

	relay := startRouteRelay(t)

	relay.route(portOf(t, healthy), healthy.Listener.Addr().String())
	relay.route(portOf(t, brokenA), brokenA.Listener.Addr().String())
	relay.route(portOf(t, brokenB), brokenB.Listener.Addr().String())

	result := connection.VerifyTunnel(context.Background(), relay.addr(), connection.VerifyOptions{
		Targets: []string{healthy.URL, brokenA.URL, brokenB.URL},
		Timeout: 4 * time.Second,
		Backoff: 50 * time.Millisecond,
	})

	if result.OK {
		t.Fatal("one of three must not reach the quorum of two")
	}

	if result.Succeeded != 1 || result.Required != 2 {
		t.Fatalf("succeeded/required = %d/%d, want 1/2", result.Succeeded, result.Required)
	}

	if result.FailureClass != connection.FailureHTTP {
		t.Fatalf("failure class = %s, want http_status (the most severe common class)", result.FailureClass)
	}
}

// TestVerifyDefaultsAndBounds pins the default target set, its quorum
// and the MaxVerifyTargets bound.
func TestVerifyDefaultsAndBounds(t *testing.T) {
	// All ports unrouted: every default target blackholes — the
	// defaults are observable through the failed result's shape.
	relay := startRouteRelay(t)

	failed := connection.VerifyTunnel(context.Background(), relay.addr(), connection.VerifyOptions{
		Timeout: 3 * time.Second,
		Backoff: 50 * time.Millisecond,
	})

	if failed.OK {
		t.Fatal("blackholed relay must fail")
	}

	if len(failed.Targets) != len(connection.DefaultVerifyTargets) {
		t.Fatalf("default target rows = %d, want %d", len(failed.Targets), len(connection.DefaultVerifyTargets))
	}

	if failed.Required != 2 {
		t.Fatalf("default quorum = %d, want 2 (majority of three)", failed.Required)
	}

	if failed.FailureClass != connection.FailureTimeout {
		t.Fatalf("blackhole class = %s, want timeout", failed.FailureClass)
	}

	// Truncation: six targets are bounded to MaxVerifyTargets.
	healthy := fakeVerifyTarget(t)

	routed := startRouteRelay(t)
	routed.route(portOf(t, healthy), healthy.Listener.Addr().String())

	six := make([]string, 6)
	for i := range six {
		six[i] = fmt.Sprintf("%s/row%d", healthy.URL, i)
	}

	bounded := connection.VerifyTunnel(context.Background(), routed.addr(), connection.VerifyOptions{
		Targets: six,
		Timeout: 3 * time.Second,
	})

	if !bounded.OK {
		t.Fatalf("bounded verification failed: %s", bounded.Describe())
	}

	if want := connection.MaxVerifyTargets; len(bounded.Targets) != want {
		t.Fatalf("target rows = %d, want the bound %d", len(bounded.Targets), want)
	}

	// A pinned single URL keeps the legacy one-target contract.
	pinned := connection.VerifyTunnel(context.Background(), routed.addr(), connection.VerifyOptions{
		URL:     healthy.URL,
		Timeout: 3 * time.Second,
	})

	if !pinned.OK || pinned.Required != 1 {
		t.Fatalf("pinned URL verification: ok=%v required=%d", pinned.OK, pinned.Required)
	}
}

// ---- transient retry ----------------------------------------------------

// TestVerifyTransientRetry5xx pins the bounded retry: a target that
// answers 500 once and 204 on the retry is verified usable, with the
// retry visible in the evidence.
func TestVerifyTransientRetry5xx(t *testing.T) {
	flake, served := countingTarget(t, http.StatusInternalServerError, http.StatusNoContent)

	relay := startRouteRelay(t)
	relay.route(portOf(t, flake), flake.Listener.Addr().String())

	result := connection.VerifyTunnel(context.Background(), relay.addr(), connection.VerifyOptions{
		URL:     flake.URL,
		Timeout: 4 * time.Second,
		Backoff: 50 * time.Millisecond,
	})

	if !result.OK {
		t.Fatalf("transient 500 must be retried into success: %s", result.Describe())
	}

	if result.Retries != 1 {
		t.Fatalf("retries = %d, want exactly one", result.Retries)
	}

	if len(result.Targets) != 1 || !result.Targets[0].Retried {
		t.Fatalf("target evidence must carry the retry: %+v", result.Targets)
	}

	if result.Targets[0].Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (initial + retry)", result.Targets[0].Attempts)
	}

	if got := served.Load(); got != 2 {
		t.Fatalf("target served %d requests, want 2 (no retry storm)", got)
	}
}

// TestVerifyDeterministic4xxNoRetry pins the other side: a 4xx is a
// deterministic objection — retrying it must never happen.
func TestVerifyDeterministic4xxNoRetry(t *testing.T) {
	objection, served404 := countingTarget(t, http.StatusNotFound)

	relay := startRouteRelay(t)
	relay.route(portOf(t, objection), objection.Listener.Addr().String())

	result := connection.VerifyTunnel(context.Background(), relay.addr(), connection.VerifyOptions{
		URL:     objection.URL,
		Timeout: 3 * time.Second,
		Backoff: 50 * time.Millisecond,
	})

	if result.OK {
		t.Fatal("404 target must fail verification")
	}

	if result.FailureClass != connection.FailureHTTP {
		t.Fatalf("failure class = %s, want http_status", result.FailureClass)
	}

	if len(result.Targets) != 1 {
		t.Fatalf("target rows = %d, want 1", len(result.Targets))
	}

	if result.Targets[0].Attempts != 1 || result.Targets[0].Retried {
		t.Fatalf("deterministic failure was retried: %+v", result.Targets[0])
	}

	if got := served404.Load(); got != 1 {
		t.Fatalf("target served %d requests, want exactly 1", got)
	}
}

// TestVerifyTransientRetryExhausted pins the retry bound from the
// exhaustion side: a blackholed target retries exactly once, then
// stands as failed with the timeout class.
func TestVerifyTransientRetryExhausted(t *testing.T) {
	relay := startRouteRelay(t) // nothing routed: timeout family

	result := connection.VerifyTunnel(context.Background(), relay.addr(), connection.VerifyOptions{
		URL:     "http://203.0.113.10:9/",
		Timeout: 3 * time.Second,
		Backoff: 50 * time.Millisecond,
	})

	if result.OK {
		t.Fatal("blackholed target must fail")
	}

	if result.FailureClass != connection.FailureTimeout {
		t.Fatalf("failure class = %s, want timeout", result.FailureClass)
	}

	if len(result.Targets) != 1 {
		t.Fatalf("target rows = %d, want 1", len(result.Targets))
	}

	if result.Targets[0].Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry, never more)", result.Targets[0].Attempts)
	}

	if !result.Targets[0].Retried {
		t.Fatal("exhausted retry must be marked retried")
	}
}

// ---- manager-level stability (§2.3) --------------------------------------

// tunnelMux is a controllable RAW TCP relay used by the stability
// tests: healthy mode pipes every connection to the upstream;
// degraded mode blackholes — exactly the "core alive, path dead"
// state the degradation model must detect through measured evidence
// (never through process health).
//
// The SOCKS5 layer terminates at the fake core, whose relay target
// is this mux's address — so the mux speaks plain TCP, not SOCKS.
type tunnelMux struct {
	ln       net.Listener
	upstream string
	mu       sync.Mutex
	healthy  bool
}

func startTunnelMux(t *testing.T, upstream string) *tunnelMux {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	mux := &tunnelMux{ln: ln, upstream: upstream, healthy: true}

	t.Cleanup(func() { _ = ln.Close() })

	go mux.serve()

	return mux
}

func (m *tunnelMux) addr() string { return m.ln.Addr().String() }

func (m *tunnelMux) setHealthy(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.healthy = v
}

func (m *tunnelMux) isHealthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.healthy
}

func (m *tunnelMux) serve() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}

		go m.handle(conn)
	}
}

func (m *tunnelMux) handle(conn net.Conn) {
	defer conn.Close()

	if !m.isHealthy() {
		// Blackhole: accept the bytes, send nothing — the client's
		// request times out while the tunnel stays "connected".
		_, _ = io.Copy(io.Discard, conn)

		return
	}

	up, err := net.Dial("tcp", m.upstream)
	if err != nil {
		return
	}

	defer up.Close()

	relayStreams(conn, up)
}

// stabilityEnv builds a manager whose session tunnels through the
// controllable mux (fake core → mux → real 204 target).
func stabilityEnv(t *testing.T, mux *tunnelMux, target string) *connection.Manager {
	t.Helper()

	t.Setenv("FAKECORE_SOCKS_RELAY", mux.addr())

	registry := raceRegistry(t)

	manager := connection.New(connection.Options{
		Registry:               registry,
		StartupTimeout:         10 * time.Second,
		MonitorInterval:        50 * time.Millisecond,
		VerifyInterval:         250 * time.Millisecond,
		VerifyGrace:            100 * time.Millisecond,
		VerifyFailureThreshold: 3,
		Verify:                 connection.VerifyPolicy{Target: target, Timeout: 2 * time.Second},
	})

	return manager
}

// TestConnectionStabilityDegradation pins the full §2.3 ladder: a
// verified session whose path dies is first marked DEGRADED (the
// session stands — one failed recheck is evidence, not a verdict),
// and only the CONSECUTIVE threshold tears it down to
// connection_failed with the degradation as the recorded reason.
func TestConnectionStabilityDegradation(t *testing.T) {
	target := fakeVerifyTarget(t)

	mux := startTunnelMux(t, target.Listener.Addr().String())

	manager := stabilityEnv(t, mux, target.URL)
	defer manager.Shutdown()

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v (state %s)", err, snapshot.State)
	}

	if snapshot.State != connection.StateConnectedVerified {
		t.Fatalf("state = %s, want connected_verified", snapshot.State)
	}

	if snapshot.Verification != "usable" {
		t.Fatalf("verification = %q, want usable", snapshot.Verification)
	}

	// The path dies (the core process stays alive).
	mux.setHealthy(false)

	// First failed recheck: degraded evidence, session standing.
	waitFor(t, 15*time.Second, "the degraded snapshot", func() bool {
		s := manager.Snapshot()
		return s.Verification == "degraded" && s.State == connection.StateConnectedVerified
	})

	// The threshold: consecutive failures tear the session down.
	waitFor(t, 30*time.Second, "connection_failed after the threshold", func() bool {
		return manager.State() == connection.StateConnectionFailed
	})

	final := manager.Snapshot()

	if !strings.Contains(final.LastError, "degraded") {
		t.Fatalf("last error = %q, want the degradation evidence", final.LastError)
	}
}

// TestConnectionStabilityRecovery pins the anti-flapping contract:
// ONE failed recheck never tears a healthy session down — the grace
// recheck that follows a restored path repairs the evidence back to
// usable with the failure counter reset.
func TestConnectionStabilityRecovery(t *testing.T) {
	target := fakeVerifyTarget(t)

	mux := startTunnelMux(t, target.Listener.Addr().String())

	manager := stabilityEnv(t, mux, target.URL)
	defer manager.Shutdown()

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v (state %s)", err, snapshot.State)
	}

	if snapshot.State != connection.StateConnectedVerified {
		t.Fatalf("state = %s, want connected_verified", snapshot.State)
	}

	// Degrade just long enough for ONE failed recheck.
	mux.setHealthy(false)

	waitFor(t, 15*time.Second, "the first failed recheck", func() bool {
		return manager.Snapshot().Verification == "degraded"
	})

	// Restore the path immediately: the session must heal, not die.
	mux.setHealthy(true)

	waitFor(t, 15*time.Second, "the recovered usable verification", func() bool {
		s := manager.Snapshot()
		return s.Verification == "usable" && s.VerifyFailures == 0
	})

	if state := manager.State(); state != connection.StateConnectedVerified {
		t.Fatalf("state = %s — a single failed recheck tore the session down", state)
	}

	// The recovery must also leave fresh timing evidence behind.
	if manager.Snapshot().VerifiedAt == 0 {
		t.Fatal("verified_at not stamped after recovery")
	}
}
