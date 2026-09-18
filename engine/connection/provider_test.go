package connection

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/provider"
	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// provider_connection_test.go verifies §11: a provider session runs
// through the SAME lifecycle (start → route → VERIFY ACTUAL INTERNET
// → connected → monitor → recover), with no duplicate process
// manager and no bypassing of verification.

// fakeProvider is a deterministic in-process provider whose local
// SOCKS endpoint actually relays traffic.
type fakeProvider struct {
	name      string
	started   bool
	stopped   bool
	failStart bool
	endpoint  net.Listener
}

func (f *fakeProvider) Name() string        { return f.name }
func (f *fakeProvider) Kind() provider.Kind { return provider.KindTor }
func (f *fakeProvider) Resolve(ctx context.Context) (provider.Release, error) {
	return provider.Release{}, nil
}
func (f *fakeProvider) Install(ctx context.Context) error   { return nil }
func (f *fakeProvider) Uninstall(ctx context.Context) error { return nil }

func (f *fakeProvider) Start(ctx context.Context) error {
	if f.failStart {
		return context.DeadlineExceeded
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	f.endpoint = listener
	f.started = true

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()

				// Minimal SOCKS5 no-auth CONNECT relay.
				buf := make([]byte, 3)
				if _, err := readFull(c, buf); err != nil {
					return
				}

				if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
					return
				}

				head := make([]byte, 4)
				if _, err := readFull(c, head); err != nil {
					return
				}

				var host string

				switch head[3] {
				case 0x01:
					addr := make([]byte, 4)
					if _, err := readFull(c, addr); err != nil {
						return
					}

					host = net.IP(addr).String()
				case 0x03:
					l := make([]byte, 1)
					if _, err := readFull(c, l); err != nil {
						return
					}

					name := make([]byte, int(l[0]))
					if _, err := readFull(c, name); err != nil {
						return
					}

					host = string(name)
				default:
					return
				}

				portBuf := make([]byte, 2)
				if _, err := readFull(c, portBuf); err != nil {
					return
				}

				target := net.JoinHostPort(host, itoa(int(portBuf[0])<<8|int(portBuf[1])))

				upstream, dialErr := net.DialTimeout("tcp", target, 3*time.Second)
				if dialErr != nil {
					_, _ = c.Write([]byte{0x05, 0x04, 0x00, 0x01, 127, 0, 0, 1, 0, 0})

					return
				}

				defer upstream.Close()

				if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}); err != nil {
					return
				}

				done := make(chan struct{}, 2)

				go func() { relay(upstream, c); done <- struct{}{} }()
				go func() { relay(c, upstream); done <- struct{}{} }()

				<-done
			}(conn)
		}
	}()

	return nil
}

func (f *fakeProvider) Stop(ctx context.Context) error {
	f.stopped = true

	if f.endpoint != nil {
		_ = f.endpoint.Close()
	}

	return nil
}

func (f *fakeProvider) State() provider.LifecycleState {
	if f.started {
		return provider.StateReady
	}

	return provider.StateInstalled
}

func (f *fakeProvider) Info() provider.Info {
	return provider.Info{
		Name:      f.name,
		Kind:      provider.KindTor,
		Installed: true,
		Version:   "0.4.8.16-fake",
		State:     f.State(),
		Endpoints: f.Endpoints(),
	}
}

func (f *fakeProvider) Endpoints() []provider.Endpoint {
	if f.endpoint == nil {
		return nil
	}

	return []provider.Endpoint{{
		Network:  "socks5",
		Host:     "127.0.0.1",
		Port:     f.endpoint.Addr().(*net.TCPAddr).Port,
		Verified: true,
	}}
}

func (f *fakeProvider) Health(ctx context.Context) provider.Health {
	alive := f.started && !f.stopped && f.endpoint != nil

	health := provider.Health{
		ProcessAlive:  alive,
		ListenerReady: alive,
		Measured:      alive,
		CheckedAt:     time.Now().UTC(),
	}

	if alive {
		health.OK = true
		health.LatencyMS = 1
	}

	return health
}

func (f *fakeProvider) Cleanup(ctx context.Context) error { return nil }

func readFull(c net.Conn, buf []byte) (int, error) {
	total := 0

	for total < len(buf) {
		n, err := c.Read(buf[total:])
		if err != nil {
			return total, err
		}

		total += n
	}

	return total, nil
}

func relay(dst, src net.Conn) {
	buf := make([]byte, 4096)

	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}

		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}

	digits := ""

	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}

	return digits
}

// providerTestManager builds a manager with an empty registry —
// provider sessions never touch the core registry.
func providerTestManager(t *testing.T) *Manager {
	t.Helper()

	m := New(Options{})

	t.Cleanup(func() { m.Shutdown() })

	return m
}

func TestConnectProviderFullLifecycle(t *testing.T) {
	manager := providerTestManager(t)

	// A real HTTP target the verification gate will fetch through the
	// provider tunnel.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Cleanup(target.Close)

	// Point verification at the local target: a REAL round trip
	// through the provider's SOCKS endpoint.
	opts := VerifyOptions{URL: target.URL, Timeout: 8 * time.Second}

	// NOTE: the default verify target is a public URL; for this test
	// the provider relays to the local server, so verification is a
	// genuine end-to-end request.
	prov := &fakeProvider{name: "tor"}

	snapshot, err := manager.ConnectProvider(context.Background(), prov, opts)
	if err != nil {
		t.Fatalf("ConnectProvider: %v", err)
	}

	if snapshot.State != StateConnected {
		t.Fatalf("state = %q, want connected", snapshot.State)
	}

	if snapshot.Core != "tor" {
		t.Fatalf("core = %q, want provider name", snapshot.Core)
	}

	if snapshot.Endpoint == "" {
		t.Fatal("snapshot must carry the provider endpoint")
	}

	// Verification is mandatory: "usable" only after a real request.
	if snapshot.Verification != "usable" {
		t.Fatalf("verification = %q, want usable", snapshot.Verification)
	}

	if snapshot.LatencyMS < 0 {
		t.Fatalf("latency = %d", snapshot.LatencyMS)
	}

	if manager.ProviderName() != "tor" {
		t.Fatal("manager must track the provider session")
	}

	// HTTP request THROUGH the provider session endpoint.
	dialer := socks5.Dialer{ProxyAddr: manager.ActiveEndpoint(), Timeout: 5 * time.Second}

	conn, err := dialer.Dial(context.Background(), "tcp", strings.TrimPrefix(target.URL, "http://"))
	if err != nil {
		t.Fatalf("dial through provider: %v", err)
	}

	_ = conn.Close()

	// Disconnect stops the provider deterministically.
	manager.Disconnect()

	if !prov.stopped {
		t.Fatal("provider must be stopped on disconnect (no orphan processes)")
	}

	if manager.ProviderName() != "" {
		t.Fatal("provider session must clear on disconnect")
	}
}

func TestConnectProviderFailedStart(t *testing.T) {
	manager := providerTestManager(t)

	prov := &fakeProvider{name: "tor", failStart: true}

	_, err := manager.ConnectProvider(context.Background(), prov, VerifyOptions{})
	if err == nil {
		t.Fatal("failed provider start must fail the connection")
	}

	snapshot := manager.Snapshot()
	if snapshot.State != StateConnectionFailed {
		t.Fatalf("state = %q, want connection_failed", snapshot.State)
	}
}

func TestConnectProviderFailedVerification(t *testing.T) {
	manager := providerTestManager(t)

	// The provider starts fine, but verification points at a dead
	// endpoint: the session must NOT be called connected.
	prov := &fakeProvider{name: "tor"}

	opts := VerifyOptions{URL: "http://127.0.0.1:1/nope", Timeout: 2 * time.Second}

	_, err := manager.ConnectProvider(context.Background(), prov, opts)
	if err == nil {
		t.Fatal("failed verification must fail the connection")
	}

	if manager.Snapshot().State != StateConnectionFailed {
		t.Fatalf("state = %q", manager.Snapshot().State)
	}

	// The half-started provider was deterministically stopped.
	if !prov.stopped {
		t.Fatal("provider must be stopped after failed verification")
	}
}

func TestReconnectProviderSession(t *testing.T) {
	manager := providerTestManager(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Cleanup(target.Close)

	prov := &fakeProvider{name: "tor"}

	opts := VerifyOptions{URL: target.URL, Timeout: 8 * time.Second}

	if _, err := manager.ConnectProvider(context.Background(), prov, opts); err != nil {
		t.Fatalf("connect: %v", err)
	}

	manager.Disconnect()

	// Reconnect re-establishes the PROVIDER session (not a config).
	snapshot, err := manager.Reconnect(context.Background())
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	if snapshot.State != StateConnected {
		t.Fatalf("state = %q, want connected", snapshot.State)
	}

	if snapshot.Core != "tor" {
		t.Fatalf("core = %q, want tor (provider session)", snapshot.Core)
	}

	manager.Disconnect()
}
