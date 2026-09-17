package connection_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
	"github.com/Parsaetak/FreeIran/system"
)

// verify_test.go exercises the v0.9.6 connection-engine VERIFY stage
// and the controlled racing capability end-to-end:
//
//   - VerifyTunnel against a REAL local SOCKS5 relay (implemented
//     below, server-side RFC 1928) into a 204 target — proving the
//     verification path measures actual usable connectivity;
//   - timeout/refused classification against black-holed and closed
//     endpoints;
//   - a full Race through the deterministic fake core running in its
//     FAKECORE_SOCKS_RELAY mode: two candidates, first verified-
//     usable wins, losing racers are cancelled cleanly.

// fakeVerifyTarget serves 204s (the connectivity proof).
func fakeVerifyTarget(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Cleanup(srv.Close)

	return srv
}

// socks5Relay is a minimal server-side SOCKS5 (RFC 1928) relay that
// forwards CONNECT requests to one fixed upstream.
type socks5Relay struct {
	ln       net.Listener
	upstream string
}

func startSocks5Relay(t *testing.T, upstream string) *socks5Relay {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	r := &socks5Relay{ln: ln, upstream: upstream}

	go r.serve()

	return r
}

func (r *socks5Relay) addr() string { return r.ln.Addr().String() }

func (r *socks5Relay) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}

		go r.handle(conn)
	}
}

func (r *socks5Relay) handle(conn net.Conn) {
	defer conn.Close()

	greet := make([]byte, 3)
	if _, err := io.ReadFull(conn, greet); err != nil {
		return
	}

	if greet[0] != 0x05 { // not SOCKS5
		return
	}

	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}

	if head[1] != 0x01 { // CONNECT only
		_, _ = conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		return
	}

	if _, err := readSocksAddr(conn, head[3]); err != nil {
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		return
	}

	up, err := net.Dial("tcp", r.upstream)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		return
	}

	defer up.Close()

	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	relayStreams(conn, up)
}

func readSocksAddr(conn net.Conn, atyp byte) (string, error) {
	readPort := func() (int, error) {
		b := make([]byte, 2)
		if _, err := io.ReadFull(conn, b); err != nil {
			return 0, err
		}

		return int(b[0])<<8 | int(b[1]), nil
	}

	switch atyp {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}

		p, err := readPort()
		if err != nil {
			return "", err
		}

		return net.IP(b).String() + ":" + strconv.Itoa(p), nil
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return "", err
		}

		host := make([]byte, l[0])
		if _, err := io.ReadFull(conn, host); err != nil {
			return "", err
		}

		p, err := readPort()
		if err != nil {
			return "", err
		}

		return string(host) + ":" + strconv.Itoa(p), nil
	default:
		return "", fmt.Errorf("unsupported atyp %d", atyp)
	}
}

func relayStreams(a, b net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()

	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()

	<-done
}

// TestVerifyTunnelSuccess drives the full verification path through a
// real local SOCKS5 relay into a 204 target.
func TestVerifyTunnelSuccess(t *testing.T) {
	target := fakeVerifyTarget(t)

	relay := startSocks5Relay(t, target.Listener.Addr().String())

	result := connection.VerifyTunnel(context.Background(), relay.addr(), connection.VerifyOptions{
		URL:     target.URL,
		Timeout: 5 * time.Second,
	})

	if !result.OK {
		t.Fatalf("verification failed: %+v (%s)", result.Metrics, result.Describe())
	}

	if result.Metrics.Status != http.StatusNoContent {
		t.Fatalf("status = %d", result.Metrics.Status)
	}

	if result.TunnelProbeMS < 0 {
		t.Fatalf("tunnel probe not measured: %d", result.TunnelProbeMS)
	}
}

// TestVerifyTunnelTimeout verifies a black-holed proxy is classified
// as a timeout.
func TestVerifyTunnelTimeout(t *testing.T) {
	// A listener that accepts and never speaks.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			defer conn.Close() //nolint:staticcheck // hold silently until test ends
		}
	}()

	result := connection.VerifyTunnel(context.Background(), ln.Addr().String(), connection.VerifyOptions{
		URL:     "https://example.com/",
		Timeout: 400 * time.Millisecond,
	})

	if result.OK {
		t.Fatal("silent proxy must fail verification")
	}

	if result.FailureClass != connection.FailureTimeout {
		t.Fatalf("failure class = %s, want timeout", result.FailureClass)
	}
}

// TestVerifyTunnelRefused verifies connection-refused classification.
func TestVerifyTunnelRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()
	ln.Close() // now refused

	result := connection.VerifyTunnel(context.Background(), addr, connection.VerifyOptions{
		URL:     "https://example.com/",
		Timeout: 2 * time.Second,
	})

	if result.OK {
		t.Fatal("refused proxy must fail")
	}

	if result.FailureClass != connection.FailureRefused {
		t.Fatalf("failure class = %s, want refused", result.FailureClass)
	}
}

// TestClassifyVerifyFailure exercises the classification table.
func TestClassifyVerifyFailure(t *testing.T) {
	cases := []struct {
		m    config.URLTestMetrics
		want connection.FailureClass
	}{
		{config.URLTestMetrics{OK: true}, connection.FailureNone},
		{config.URLTestMetrics{Error: "timeout", Timeout: true}, connection.FailureTimeout},
		{config.URLTestMetrics{Error: "refused"}, connection.FailureRefused},
		{config.URLTestMetrics{Error: "reset"}, connection.FailureReset},
		{config.URLTestMetrics{Error: "proxy"}, connection.FailureProxyHand},
		{config.URLTestMetrics{Error: "tls: handshake failure"}, connection.FailureTLS},
		{config.URLTestMetrics{Status: 502, Error: "HTTP 502 through tunnel"}, connection.FailureHTTP},
	}

	for _, tc := range cases {
		if got := connection.ClassifyVerifyFailure(tc.m); got != tc.want {
			t.Fatalf("classify(%q) = %s, want %s", tc.m.Error, got, tc.want)
		}
	}
}

// --- Racing ----------------------------------------------------------

// raceRegistry builds a registry with the fake v2ray staged (the
// repo-standard deterministic fixture).
func raceRegistry(t *testing.T) *core.Registry {
	t.Helper()

	dir := t.TempDir()

	contract.StageFakeCore(t, dir, "v2ray")

	registry := core.NewRegistry(system.NewCoreLocator(dir))

	if err := registry.Register(v2ray.New(), 1); err != nil {
		t.Fatalf("register v2ray: %v", err)
	}

	if err := registry.Register(xray.New(), 0); err != nil {
		t.Fatalf("register xray: %v", err)
	}

	registry.Refresh(context.Background())

	return registry
}

// TestRaceNoCandidates verifies the fast-fail path.
func TestRaceNoCandidates(t *testing.T) {
	_, err := connection.Race(context.Background(), connection.RaceOptions{
		Registry:   core.NewRegistry(nil),
		Candidates: nil,
	})

	if err == nil {
		t.Fatal("race with no candidates must fail")
	}
}

// TestRaceRegistryMissing verifies the registry requirement.
func TestRaceRegistryMissing(t *testing.T) {
	_, err := connection.Race(context.Background(), connection.RaceOptions{
		Candidates: []config.Config{{Type: config.TypeVLESS, Address: "a", Port: 1}},
	})

	if err != connection.ErrRaceRegistryMissing {
		t.Fatalf("err = %v, want ErrRaceRegistryMissing", err)
	}
}

// TestRaceCancellation ends promptly under a cancelled parent.
func TestRaceCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := connection.Race(ctx, connection.RaceOptions{
		Registry: core.NewRegistry(nil),
		Candidates: []config.Config{
			{Type: config.TypeVLESS, Address: "203.0.113.1", Port: 443},
			{Type: config.TypeVLESS, Address: "203.0.113.2", Port: 443},
		},
		Racers: 2,
	})

	if err == nil {
		t.Fatal("cancelled race must fail")
	}
}

// TestRaceWithFakeCores drives a REAL race: the fake cores run in
// FAKECORE_SOCKS_RELAY mode so every candidate's tunnel genuinely
// forwards to a local 204 target; the first verified-usable racer
// wins and the loser is cancelled cleanly.
func TestRaceWithFakeCores(t *testing.T) {
	target := fakeVerifyTarget(t)

	t.Setenv("FAKECORE_SOCKS_RELAY", target.Listener.Addr().String())

	registry := raceRegistry(t)

	candidates := make([]config.Config, 0, 3)

	for i := 0; i < 3; i++ {
		cfg := config.Config{
			Type:    config.TypeVLESS,
			Address: "127.0.0.1",
			Port:    443,
			UUID:    fmt.Sprintf("00000000-0000-0000-0000-%012d", 100+i),
			Network: "tcp",
		}
		cfg.Normalize()

		candidates = append(candidates, cfg)
	}

	outcome, err := connection.Race(context.Background(), connection.RaceOptions{
		Candidates: candidates,
		Racers:     3,
		Registry:   registry,
		Verify:     connection.VerifyOptions{URL: target.URL, Timeout: 6 * time.Second},
	})

	if err != nil {
		for _, a := range outcome.Attempts {
			t.Logf("attempt %+v", a)
		}

		t.Fatalf("race failed: %v", err)
	}

	if outcome.Winner == nil {
		t.Fatal("race produced no winner")
	}

	found := false

	for _, a := range outcome.Attempts {
		if a.ConfigFingerprint == outcome.Winner.Fingerprint() {
			found = true

			if !a.OK {
				t.Fatal("winner's attempt must be OK")
			}
		}
	}

	if !found {
		t.Fatal("winner not present in attempts")
	}

	if outcome.WinnerVerify.Metrics.Status != http.StatusNoContent {
		t.Fatalf("winner verification status = %d", outcome.WinnerVerify.Metrics.Status)
	}

	if outcome.DurationMS < 0 {
		t.Fatalf("duration not measured: %d", outcome.DurationMS)
	}
}

// TestRaceAllFail verifies the exhausted outcome when every racer
// fails verification (black-holed relay target).
func TestRaceAllFail(t *testing.T) {
	// A SOCKS relay whose upstream is a silent listener: the SOCKS
	// handshake succeeds, but the HTTP request through the tunnel
	// times out — verification fails with a timeout classification.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer silent.Close()

	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}

			defer conn.Close() //nolint:staticcheck // hold silently
		}
	}()

	t.Setenv("FAKECORE_SOCKS_RELAY", silent.Addr().String())

	registry := raceRegistry(t)

	candidates := make([]config.Config, 0, 2)

	for i := 0; i < 2; i++ {
		cfg := config.Config{
			Type:    config.TypeVLESS,
			Address: "127.0.0.1",
			Port:    443,
			UUID:    fmt.Sprintf("00000000-0000-0000-0000-%012d", 200+i),
			Network: "tcp",
		}
		cfg.Normalize()

		candidates = append(candidates, cfg)
	}

	outcome, err := connection.Race(context.Background(), connection.RaceOptions{
		Candidates: candidates,
		Racers:     2,
		Registry:   registry,
		Verify:     connection.VerifyOptions{URL: "https://example.com/", Timeout: 2 * time.Second},
	})

	if err == nil {
		t.Fatal("race with only failing candidates must fail")
	}

	if outcome.Winner != nil {
		t.Fatal("no winner expected")
	}

	for _, a := range outcome.Attempts {
		if a.OK {
			t.Fatalf("attempt unexpectedly OK: %+v", a)
		}
	}
}
