package freecore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// echoTarget is a raw TCP echo server (no t.Cleanup wrapper for use
// inside fixture builders).
func echoTarget(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	return ln.Addr().String()
}

// localSOCKS5Remote runs a REAL local SOCKS5 remote (the in-repo
// reviewed server shape from the socks5 tests) the engine forwards
// through — the full first-party path with a genuine proxy hop.
func localSOCKS5Remote(t *testing.T) string {
	t.Helper()

	target := echoTarget(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("remote listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) { serveLocalRemote(c, target) }(conn)
		}
	}()

	return ln.Addr().String()
}

// serveLocalRemote implements the minimal no-auth SOCKS5 server.
func serveLocalRemote(conn net.Conn, target string) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 0x05 {
		return
	}

	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}

	_, _ = conn.Write([]byte{0x05, 0x00})

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil || req[1] != 0x01 {
		return
	}

	var skip []byte

	switch req[3] {
	case 0x01:
		skip = make([]byte, 4)
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return
		}

		skip = make([]byte, int(lenBuf[0]))
	case 0x04:
		skip = make([]byte, 16)
	default:
		return
	}

	if _, err := io.ReadFull(conn, skip); err != nil {
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}

	_ = conn.SetDeadline(time.Time{})

	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		return
	}

	defer upstream.Close()

	_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	go func() { _, _ = io.Copy(upstream, conn) }()
	_, _ = io.Copy(conn, upstream)
}

// remoteSocksConfig builds a SOCKS configuration pointing at the
// local remote.
func remoteSocksConfig(t *testing.T) config.Config {
	t.Helper()

	host, portStr, _ := net.SplitHostPort(localSOCKS5Remote(t))

	var port int

	_, _ = fmtSscan(portStr, &port)

	return config.Config{
		ID:      "it-socks",
		Type:    config.TypeSOCKS,
		Address: host,
		Port:    port,
	}
}

// TestBackendContract exercises the core.Core surface of the
// first-party backend: capability gating, deterministic documents and
// the in-process start.
func TestBackendContract(t *testing.T) {
	backend := New()

	if backend.Name() != "freecore" {
		t.Fatalf("name = %q", backend.Name())
	}

	if DisplayName != "FreeIran Engine" {
		t.Fatalf("display name = %q", DisplayName)
	}

	cfg := remoteSocksConfig(t)

	if !backend.Supports(cfg) {
		t.Fatal("socks/plain-tcp config must be supported")
	}

	if err := backend.Validate(context.Background(), cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Unsupported shapes are refused.
	vless := cfg
	vless.Type = config.TypeVLESS

	if backend.Supports(vless) {
		t.Fatal("vless must NOT be supported")
	}

	if err := backend.Validate(context.Background(), vless); err == nil {
		t.Fatal("validate must refuse vless")
	}

	tlsCfg := cfg
	tlsCfg.Security = "tls"

	if backend.Supports(tlsCfg) {
		t.Fatal("tls must NOT be supported")
	}

	// Deterministic documents.
	doc1, err := backend.BuildConfig(cfg, core.RuntimeOptions{LocalPort: 10808})
	if err != nil {
		t.Fatalf("build config: %v", err)
	}

	doc2, err := backend.BuildConfig(cfg, core.RuntimeOptions{LocalPort: 10808})
	if err != nil {
		t.Fatalf("build config 2: %v", err)
	}

	if string(doc1.Data) != string(doc2.Data) {
		t.Fatal("document generation must be deterministic")
	}

	var descriptor map[string]any
	if err := json.Unmarshal(doc1.Data, &descriptor); err != nil {
		t.Fatalf("descriptor is not valid JSON: %v", err)
	}

	if descriptor["engine"] != DisplayName {
		t.Fatalf("descriptor engine = %v", descriptor["engine"])
	}
}

// TestBackendStartInProcess proves the central v0.13.1 property: the
// first-party backend starts an in-process engine — real listener,
// real bytes, NO managed process — and the instance lifecycle follows
// the ONE core contract.
func TestBackendStartInProcess(t *testing.T) {
	cfg := remoteSocksConfig(t)

	backend := New()

	instance, err := backend.Start(context.Background(), cfg, core.RuntimeOptions{
		LocalPort:      0, // ephemeral — resolved by the shared authority
		StartupTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	defer instance.Close()

	// THE no-external-process proof: an in-process instance has no
	// child (PID 0 by construction — nothing was spawned).
	if instance.PID() != 0 {
		t.Fatalf("in-process instance PID = %d, want 0 (no child process)", instance.PID())
	}

	if instance.CoreName() != "freecore" {
		t.Fatalf("core name = %q", instance.CoreName())
	}

	if instance.Endpoint() == "" {
		t.Fatal("instance must expose the engine-owned local endpoint")
	}

	// Readiness: the shared launch verdict (listener probe).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := instance.WaitReady(ctx); err != nil {
		t.Fatalf("wait ready: %v", err)
	}

	// Health: in-process aliveness + ready listener.
	report := instance.Health(ctx)
	if !report.ProcessAlive || !report.ListenerReady {
		t.Fatalf("health = %+v, want alive in-process session with ready listener", report)
	}

	if !instance.Alive() {
		t.Fatal("in-process instance must report alive while running")
	}

	// Real bytes through the first-party path: SOCKS5 inbound →
	// SOCKS5 outbound → local remote → echo target.
	dialer := socks5.Dialer{ProxyAddr: instance.Endpoint(), Timeout: 3 * time.Second}

	echoAddr := echoTarget(t)

	conn, err := dialer.Dial(ctx, "tcp", echoAddr)
	if err != nil {
		t.Fatalf("dial through engine: %v", err)
	}

	defer conn.Close()

	payload := []byte("first-party path carries real bytes")

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	echo := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	if string(echo) != string(payload) {
		t.Fatalf("echo = %q, want %q", echo, payload)
	}
}

// stubExternalCore is a registry-registered stand-in proving
// selection routing: the first-party backend wins supported routes
// and external cores keep everything else.
type stubExternalCore struct {
	name    string
	started int
}

func (s *stubExternalCore) Name() string { return s.name }

func (s *stubExternalCore) Supports(cfg config.Config) bool { return true }

func (s *stubExternalCore) Validate(context.Context, config.Config) error { return nil }

func (s *stubExternalCore) BuildConfig(config.Config, core.RuntimeOptions) (core.RuntimeConfig, error) {
	return core.RuntimeConfig{FileName: "stub.json", Data: []byte("{}"), Format: "stub"}, nil
}

func (s *stubExternalCore) Start(context.Context, config.Config, core.RuntimeOptions) (*core.Instance, error) {
	s.started++

	return nil, errors.New("stub: no real process — selection-routing test only")
}

// Availability makes the stub behave like an installed external core
// for the registry (static availability instead of discovery).
func (s *stubExternalCore) Availability() (core.BackendStatus, string, string, string, string, string) {
	return core.StatusAvailable, "26.0.0-test", "/bin/stub", "", "path", "external"
}

func TestRegistrySelectionRouting(t *testing.T) {
	registry := core.NewRegistry(nil)
	_ = registry.Register(New(), 0)

	external := &stubExternalCore{name: "xray"}
	_ = registry.Register(external, 0)

	registry.Refresh(context.Background())

	// Supported route: the first-party engine wins (priority tie,
	// deterministic name ordering: freecore < xray).
	supported := remoteSocksConfig(t)

	sel, err := registry.Select(supported, core.Preferences{})
	if err != nil {
		t.Fatalf("select supported: %v", err)
	}

	if sel.Core.Name() != "freecore" {
		t.Fatalf("supported route selected %q, want freecore", sel.Core.Name())
	}

	// Unsupported route: the external core keeps it (honest
	// compatibility fallback).
	unsupported := supported
	unsupported.Type = config.TypeVLESS

	sel, err = registry.Select(unsupported, core.Preferences{})
	if err != nil {
		t.Fatalf("select unsupported: %v", err)
	}

	if sel.Core.Name() != "xray" {
		t.Fatalf("unsupported route selected %q, want the external core", sel.Core.Name())
	}

	// v0.14.0 first-party ownership: an ordinary external preference
	// no longer launches an external core for a route the FreeIran
	// Engine genuinely supports.
	prefSel, err := registry.Select(supported, core.Preferences{PreferredBackend: "xray"})
	if err != nil {
		t.Fatalf("select preferred: %v", err)
	}

	if prefSel.Core.Name() != "freecore" {
		t.Fatalf("supported route with external preference = %q, want freecore (first-party ownership)", prefSel.Core.Name())
	}

	if sel, err := registry.Select(unsupported, core.Preferences{PreferredBackend: "xray"}); err != nil || sel.Core.Name() != "xray" {
		t.Fatalf("unsupported route with preference = %v/%q, want xray (preference rules among external cores)", err, sel.Core.Name())
	}
}

// TestBackendStaticAvailability proves the registry seam: the
// in-process engine reports itself available with the application
// version — no executable discovery, no fake path.
func TestBackendStaticAvailability(t *testing.T) {
	backend := New()

	var static core.StaticAvailability = backend

	status, version, path, note, origin, ownership := static.Availability()

	if status != core.StatusAvailable {
		t.Fatalf("status = %q, want available", status)
	}

	if version == "" {
		t.Fatal("the engine reports the application version — empty is wrong")
	}

	if path != "" {
		t.Fatalf("in-process backend path = %q, want empty (no binary)", path)
	}

	if note == "" || origin != "builtin" || ownership != "first-party" {
		t.Fatalf("availability = %q / %q / %q", note, origin, ownership)
	}
}
