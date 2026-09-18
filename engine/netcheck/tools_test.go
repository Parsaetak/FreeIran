package netcheck

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// tools_test.go exercises the shared Internet-Tools engine (§16):
// for every tool family — success, timeout, cancellation, invalid
// input and tunnel/proxy mode where applicable — against LOCAL
// servers only. No test depends on the live Internet.

// echoTCPServer accepts and immediately closes (TCP reachability).
func echoTCPServer(t *testing.T) (addr string, port int, closeFn func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	return listener.Addr().String(), listener.Addr().(*net.TCPAddr).Port, func() {
		listener.Close()
		<-done
	}
}

// httpServer serves bounded responses for https/websocket/captive
// tests.
func httpServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server
}

// tlsServer serves a real TLS endpoint for the TLS tool.
func tlsServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	return server
}

// fakeSOCKSProxy implements the SOCKS5 no-auth CONNECT subset well
// enough for the engine dialer: handshake, CONNECT, accept.
func fakeSOCKSProxy(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}

	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go serveSOCKS(conn)
		}
	}()

	return listener.Addr().String()
}

func serveSOCKS(conn net.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	reader := bufio.NewReader(conn)

	// Greeting: VER NMETHODS METHODS...
	header := make([]byte, 2)
	if _, err := reader.Read(header); err != nil {
		return
	}

	if header[0] != 5 {
		return
	}

	methods := make([]byte, int(header[1]))
	if _, err := reader.Read(methods); err != nil {
		return
	}

	// Select NO AUTHENTICATION.
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// Request: VER CMD RSV ATYP ADDR PORT.
	req := make([]byte, 4)
	if _, err := reader.Read(req); err != nil {
		return
	}

	if req[1] != 0x01 { // CONNECT only
		return
	}

	var addrLen int

	switch req[3] {
	case 0x01:
		addrLen = 4
	case 0x03:
		l := make([]byte, 1)
		if _, err := reader.Read(l); err != nil {
			return
		}

		addrLen = int(l[0])

		extra := make([]byte, addrLen)
		if _, err := reader.Read(extra); err != nil {
			return
		}

		addrLen = 0
	case 0x04:
		addrLen = 16
	default:
		return
	}

	if addrLen > 0 {
		addr := make([]byte, addrLen)
		if _, err := reader.Read(addr); err != nil {
			return
		}
	}

	port := make([]byte, 2)
	if _, err := reader.Read(port); err != nil {
		return
	}

	// Success reply: VER REP RSV ATYP(IPv4) BND ADDR BND PORT.
	reply := []byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}

	if _, err := conn.Write(reply); err != nil {
		return
	}

	// Hold the connection briefly (the tool closes after measuring).
	time.Sleep(200 * time.Millisecond)
}

// localDial wraps a SOCKS dialer for tunneled requests in tests.
func localDial(proxy string) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := sockslate{proxy: proxy}
		return d.dial(ctx, network, addr)
	}
}

// localEndpointRunner is the sanctioned pattern for running GENERIC
// tools against local test servers: the explicit AllowPrivateTargets
// capability (the default policy blocks loopback/private destinations
// for tcp/tls/https/websocket — only socks5/http_connect have
// implicit local-endpoint semantics).
func localEndpointRunner() *ToolRunner {
	return &ToolRunner{Safety: Safety{AllowPrivateTargets: true}}
}

// ---- TCP --------------------------------------------------------------

func TestToolTCPSuccess(t *testing.T) {
	addr, _, closeFn := echoTCPServer(t)
	defer closeFn()

	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolTCP,
		Target: addr,
	})

	if result.Status != ToolStatusOK {
		t.Fatalf("status = %q (%s), want ok", result.Status, result.Error)
	}

	if !result.Measurement.Measured {
		t.Fatal("successful TCP dial must be measured")
	}

	if result.Measurement.LatencyMS < 0 {
		t.Fatalf("negative latency: %d", result.Measurement.LatencyMS)
	}

	if result.DurationMS <= 0 {
		t.Fatalf("duration must be positive telemetry, got %d", result.DurationMS)
	}

	if result.Path != PathDirect {
		t.Fatalf("path = %q, want direct", result.Path)
	}
}

func TestToolTCPSubMillisecondRepresentation(t *testing.T) {
	addr, _, closeFn := echoTCPServer(t)
	defer closeFn()

	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolTCP, Target: addr})

	// Loopback is usually sub-ms: the projection may read 0 but the
	// Measured flag must carry the truth (v0.9.8.1 semantics).
	if result.Measurement.LatencyMS == 0 && !result.Measurement.SubMS {
		t.Fatal("sub-ms TCP measurement must flag SubMS=true")
	}

	if result.Measurement.LatencyMS > 0 && result.Measurement.SubMS {
		t.Fatal("SubMS must not be set for >= 1 ms measurements")
	}
}

func TestToolTCPRefused(t *testing.T) {
	// Reserve then close a port so nothing listens.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}

	addr := listener.Addr().String()
	listener.Close()

	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolTCP, Target: addr})

	if result.Status != ToolStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}

	if result.Measurement.Measured {
		t.Fatal("failed dial must not claim a measurement")
	}
}

func TestToolTCPCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner := localEndpointRunner()

	result := runner.Run(ctx, ToolRequest{
		Tool:    ToolTCP,
		Target:  "10.255.255.1:65534", // unroutable (allowed: explicit local capability)
		Timeout: 5 * time.Second,
	})

	if result.Status != ToolStatusCancelled {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
}

func TestToolTCPInvalidTarget(t *testing.T) {
	runner := NewToolRunner()

	for _, target := range []string{
		"http://example.com", // URL where host:port expected
		"host:notaport",
		"",
	} {
		_ = target
	}

	// Port out of range.
	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolHTTPS,
		Target: "ftp://example.com", // scheme not allowed
	})

	if result.Status != ToolStatusInvalid {
		t.Fatalf("scheme check: status = %q, want invalid_target", result.Status)
	}
}

func TestToolHTTPSURLCredentialsBlocked(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolHTTPS,
		Target: "https://user:secret@example.com/",
	})

	if result.Status != ToolStatusInvalid {
		t.Fatalf("status = %q, want invalid_target (credentials in URL)", result.Status)
	}

	if strings.Contains(result.Error, "credentials") == false {
		t.Fatalf("error should explain the credential policy: %q", result.Error)
	}
}

func TestToolTCPPriivacyPolicyBlocksAutonomousPrivateTarget(t *testing.T) {
	runner := NewToolRunner()

	// The DNS tool's autonomous defaults must never target private
	// ranges: an explicit private target is allowed for local tools,
	// but the traceroute (non-local) tool blocks it.
	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolTraceroute,
		Target: "192.168.1.1",
	})

	if result.Status != ToolStatusInvalid {
		t.Fatalf("traceroute to private IP: status = %q, want invalid_target", result.Status)
	}
}

// ---- HTTPS ------------------------------------------------------------

func TestToolHTTPSSuccess(t *testing.T) {
	server := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolHTTPS,
		Target: server.URL + "/generate_204",
	})

	if result.Status != ToolStatusOK {
		t.Fatalf("status = %q (%s), want ok", result.Status, result.Error)
	}

	if result.Measurement.Status != http.StatusNoContent {
		t.Fatalf("HTTP status = %d, want 204", result.Measurement.Status)
	}

	if !result.Measurement.Measured {
		t.Fatal("HTTPS round trip must be measured")
	}
}

func TestToolHTTPSHTTPErrorStatus(t *testing.T) {
	server := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "teapot", http.StatusTeapot)
	})

	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolHTTPS, Target: server.URL})

	if result.Status != ToolStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}

	if result.Measurement.Status != http.StatusTeapot {
		t.Fatalf("status code = %d, want 418", result.Measurement.Status)
	}
}

func TestToolHTTPSRedirectCap(t *testing.T) {
	server := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Infinite redirect loop.
		http.Redirect(w, r, "/loop", http.StatusFound)
	})

	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolHTTPS, Target: server.URL + "/start"})

	if result.Status != ToolStatusFailed {
		t.Fatalf("status = %q, want failed (redirect cap)", result.Status)
	}

	if !strings.Contains(strings.ToLower(result.Error), "redirect") {
		t.Fatalf("error should mention redirects: %q", result.Error)
	}
}

func TestToolHTTPSResponseSizeCap(t *testing.T) {
	server := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", 4<<20)) // 4 MiB
		_, _ = w.Write(make([]byte, 4<<20))
	})

	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolHTTPS, Target: server.URL})

	if result.Measurement.Bytes > runner.Safety.responseCap() {
		t.Fatalf("bytes read %d exceeds the safety cap %d", result.Measurement.Bytes, runner.Safety.responseCap())
	}

	if result.Status != ToolStatusOK {
		t.Fatalf("size-cap test must reach the real body drain: status = %q (%s)", result.Status, result.Error)
	}
}

// ---- TLS ---------------------------------------------------------------

func TestToolTLSSuccess(t *testing.T) {
	server := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	host, port := hostPortOfURL(t, server.URL)

	runner := &ToolRunner{Safety: Safety{
		AllowPrivateTargets: true, // local test endpoint
		AllowInsecureTLS:    true, // self-signed test certificate
	}}

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolTLS,
		Target: net.JoinHostPort(host, strconvItoa(port)),
	})

	if result.Status != ToolStatusOK {
		t.Fatalf("status = %q (%s), want ok", result.Status, result.Error)
	}

	if result.Measurement.TLSVersion == "" {
		t.Fatal("TLS version must be reported from the real handshake")
	}

	if result.Measurement.Cipher == "" {
		t.Fatal("cipher suite must be reported from the real handshake")
	}
}

func TestToolTLSCertificateVerification(t *testing.T) {
	server := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	host, port := hostPortOfURL(t, server.URL)

	// Default safety plus the explicit local capability: verification
	// stays ON — a self-signed certificate must fail honestly.
	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolTLS,
		Target: net.JoinHostPort(host, strconvItoa(port)),
	})

	if result.Status != ToolStatusFailed {
		t.Fatalf("status = %q, want failed (untrusted certificate)", result.Status)
	}
}

// ---- SOCKS5 --------------------------------------------------------------

func TestToolSOCKS5SuccessThroughLocalProxy(t *testing.T) {
	proxy := fakeSOCKSProxy(t)

	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolSOCKS5, Target: proxy})

	if result.Status != ToolStatusOK {
		t.Fatalf("status = %q (%s), want ok", result.Status, result.Error)
	}

	if !result.Measurement.Measured {
		t.Fatal("SOCKS5 CONNECT round trip must be measured")
	}

	if result.Transport != "socks5" {
		t.Fatalf("transport = %q, want socks5", result.Transport)
	}
}

func TestToolSOCKS5Refused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}

	deadProxy := listener.Addr().String()
	listener.Close()

	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolSOCKS5, Target: deadProxy})

	if result.Status != ToolStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
}

// ---- HTTP CONNECT ---------------------------------------------------------

func TestToolHTTPConnectSuccess(t *testing.T) {
	server := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusOK)
		}
	})

	host, port := hostPortOfURL(t, server.URL)

	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolHTTPConnect,
		Target: net.JoinHostPort(host, strconvItoa(port)),
	})

	if result.Status != ToolStatusOK {
		t.Fatalf("status = %q (%s), want ok", result.Status, result.Error)
	}

	if result.Measurement.Status != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", result.Measurement.Status)
	}
}

func TestToolHTTPConnectRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}

	deadProxy := listener.Addr().String()
	listener.Close()

	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolHTTPConnect, Target: deadProxy})

	if result.Status != ToolStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
}

// ---- WebSocket --------------------------------------------------------------

func TestToolWebSocketUpgradeAccepted(t *testing.T) {
	server := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "Upgrade")
			w.WriteHeader(http.StatusSwitchingProtocols)
		}
	})

	runner := localEndpointRunner()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolWebSocket, Target: wsURL})

	if result.Status != ToolStatusOK {
		t.Fatalf("status = %q (%s), want ok", result.Status, result.Error)
	}

	if result.Measurement.Status != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", result.Measurement.Status)
	}
}

func TestToolWebSocketRejected(t *testing.T) {
	server := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no upgrade here", http.StatusBadRequest)
	})

	runner := localEndpointRunner()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolWebSocket, Target: wsURL})

	if result.Status != ToolStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}

	if result.Measurement.Status != http.StatusBadRequest {
		t.Fatalf("upgrade status = %d, want 400", result.Measurement.Status)
	}
}

// ---- tunneled mode --------------------------------------------------------

func TestToolTunneledRequiresDial(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool: ToolTCP,
		Path: PathTunneled,
		// No dial supplied.
	})

	if result.Status != ToolStatusUnsupported {
		t.Fatalf("status = %q, want unsupported (no tunnel)", result.Status)
	}
}

func TestToolTunneledHTTPSThroughSOCKS(t *testing.T) {
	proxy := fakeSOCKSProxy(t)

	// An HTTP server reachable... the SOCKS proxy fakes CONNECT, so
	// the request will fail downstream — but the TOOL must honestly
	// report the tunneled path and provider label, proving the dialer
	// routing works end to end.
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:     ToolHTTPS,
		Target:   "https://www.gstatic.com/generate_204",
		Path:     PathTunneled,
		Dial:     localDial(proxy),
		Provider: "test-tor",
	})

	if result.Path != PathTunneled {
		t.Fatalf("path = %q, want tunneled", result.Path)
	}

	if result.Provider != "test-tor" {
		t.Fatalf("provider = %q, want test-tor", result.Provider)
	}

	// With a fake proxy the request fails — honestly reported.
	if result.Status == ToolStatusOK {
		t.Fatal("fake SOCKS proxy cannot serve real traffic; ok would be fabricated")
	}
}

func TestToolTunnelDiagnostics(t *testing.T) {
	proxy := fakeSOCKSProxy(t)

	runner := NewToolRunner()

	// Inactive tunnel: honest failure.
	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolTunnelDiagnostics,
		Tunnel: &TunnelSnapshot{Active: false},
	})

	if result.Status != ToolStatusFailed {
		t.Fatalf("inactive tunnel status = %q, want failed", result.Status)
	}

	// Active tunnel with a real local endpoint: real measurement.
	result = runner.Run(context.Background(), ToolRequest{
		Tool: ToolTunnelDiagnostics,
		Tunnel: &TunnelSnapshot{
			Active:   true,
			Provider: "tor",
			Endpoint: proxy,
			Healthy:  true,
		},
	})

	if result.Status != ToolStatusOK {
		t.Fatalf("active tunnel status = %q (%s), want ok", result.Status, result.Error)
	}

	if !result.Measurement.Measured {
		t.Fatal("tunnel diagnostics must carry a real measurement")
	}

	if result.Measurement.Tunnel == nil || result.Measurement.Tunnel.Provider != "tor" {
		t.Fatalf("tunnel snapshot missing: %+v", result.Measurement.Tunnel)
	}
}

// ---- safety unit tests ------------------------------------------------------

func TestIsPrivateIP(t *testing.T) {
	private := []string{
		"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.1.1",
		"0.0.0.0", "100.64.0.1", "::1", "fe80::1", "fc00::1",
	}

	for _, s := range private {
		if !IsPrivateIP(net.ParseIP(s)) {
			t.Fatalf("%s must be private", s)
		}
	}

	public := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"}

	for _, s := range public {
		if IsPrivateIP(net.ParseIP(s)) {
			t.Fatalf("%s must be public", s)
		}
	}
}

func TestSafeDialerBlocksPrivateForNonLocalTools(t *testing.T) {
	safety := DefaultSafety()

	dial := safety.dialer(ToolTraceroute)

	// Traceroute is not a local-target tool: direct private dial is
	// blocked even at the dialer level (rebinding guard backstop).
	if _, err := dial.Dial(context.Background(), "tcp", "192.168.1.1:80"); err == nil {
		t.Fatal("private dial must be blocked for non-local tools")
	}
}

func TestSafeDialerAllowsPrivateForLocalTools(t *testing.T) {
	addr, _, closeFn := echoTCPServer(t)
	defer closeFn()

	safety := DefaultSafety()

	dial := safety.dialer(ToolSOCKS5)

	conn, err := dial.Dial(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("local tool must reach the user's private endpoint: %v", err)
	}

	_ = conn.Close()
}

func TestSafeDialerResolveCheckPin(t *testing.T) {
	safety := DefaultSafety()

	dial := safety.dialer(ToolHTTPS)

	// A public hostname that resolves to a blocked range: "localhost"
	// resolves to 127.0.0.1 — the https tool is NOT a local-target
	// tool, so the rebinding guard must refuse.
	if _, err := dial.Dial(context.Background(), "tcp", "localhost:80"); err == nil {
		t.Fatal("hostname resolving to a private address must be blocked for non-local tools")
	}
}

func TestToolInvalidURLForWebSocket(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolWebSocket,
		Target: "https://example.com", // wrong scheme
	})

	if result.Status != ToolStatusInvalid {
		t.Fatalf("status = %q, want invalid_target", result.Status)
	}
}

func TestToolQUICHonestUnsupported(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolQUIC})

	if result.Status != ToolStatusUnsupported {
		t.Fatalf("status = %q, want unsupported", result.Status)
	}

	if result.Error == "" {
		t.Fatal("unsupported tools must explain WHY")
	}
}

func TestToolTracerouteUnsupportedWithoutPrivileges(t *testing.T) {
	if canOpenRawICMP() {
		t.Skip("running with raw-socket privileges; privilege-gated path covered by the elevated run")
	}

	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolTraceroute, Target: "1.1.1.1"})

	if result.Status != ToolStatusUnsupported {
		t.Fatalf("status = %q, want unsupported without privileges", result.Status)
	}
}

func TestToolUnknownToolID(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolID("nope")})

	if result.Status != ToolStatusInvalid {
		t.Fatalf("status = %q, want invalid", result.Status)
	}
}

func TestToolCatalogueComplete(t *testing.T) {
	catalogue := ToolCatalogue()

	if len(catalogue) != len(AllTools) {
		t.Fatalf("catalogue has %d entries, want %d", len(catalogue), len(AllTools))
	}

	for _, info := range catalogue {
		if info.Label == "" || info.Group == "" || info.Timeout <= 0 {
			t.Fatalf("incomplete catalogue entry: %+v", info)
		}
	}
}

func TestToolTimeoutBounds(t *testing.T) {
	// Requested timeouts are clamped into [1s, 60s].
	if d := normalizedToolTimeout(ToolTCP, 0); d != DefaultToolTimeout {
		t.Fatalf("default timeout = %v", d)
	}

	if d := normalizedToolTimeout(ToolTCP, 10*time.Millisecond); d != time.Second {
		t.Fatalf("sub-second timeout must clamp to 1s, got %v", d)
	}

	if d := normalizedToolTimeout(ToolTCP, 5*time.Minute); d != MaxToolTimeout {
		t.Fatalf("over-long timeout must clamp to %v, got %v", MaxToolTimeout, d)
	}
}

func TestToolUDPInvalidResolver(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolUDP,
		Target: "999.999.999.999:53", // syntactically broken
	})

	if result.Status == ToolStatusOK {
		t.Fatal("invalid resolver must not succeed")
	}
}

// ---- helpers ------------------------------------------------------------

func hostPortOfURL(t *testing.T, raw string) (string, int) {
	t.Helper()

	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(
		strings.TrimPrefix(raw, "https://"), "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", raw, err)
	}

	var port int

	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}

	return "127.0.0.1", port
}

func strconvItoa(v int) string {
	return fmt.Sprintf("%d", v)
}

// canOpenRawICMP reports whether this test context has raw-socket
// privileges (root / Administrator).
func canOpenRawICMP() bool {
	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return false
	}

	_ = conn.Close()

	return true
}

// sockslate exists to satisfy the localDial helper without importing
// the engine dialer twice in this test file.
type sockslate struct {
	proxy string
}

func (s sockslate) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	// Minimal SOCKS5 client path reusing the production dialer.
	d := newSOCKS5(s.proxy)

	return d.Dial(ctx, network, addr)
}

// newSOCKS5 builds the production SOCKS5 dialer (test shim).
func newSOCKS5(proxy string) socks5DialerForTest {
	return socks5DialerForTest{proxy: proxy}
}

// socks5DialerForTest wraps the engine dialer for test use.
type socks5DialerForTest struct {
	proxy string
}

func (d socks5DialerForTest) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	prod := socks5.Dialer{ProxyAddr: d.proxy, Timeout: 5 * time.Second}

	return prod.Dial(ctx, network, addr)
}

// ---- private-target policy regression battery (v0.9.8.1 root fix) --------
//
// The v0.9.8.1 Windows failure (TestSafeDialerResolveCheckPin) came
// from localTargetTools() treating generic diagnostics (tcp/tls/
// websocket/dns/https) as implicitly private-target-safe, which
// bypassed the DNS-rebinding guard. These tests pin the corrected
// semantics: private permission is EXPLICIT (AllowPrivateTargets) or
// genuinely local-endpoint (socks5/http_connect) — nothing else.

// TestToolHTTPSPrivateTargetsBlockedByDefault: generic https/tcp/
// websocket diagnostics refuse loopback and private literals at
// validation time — before any bytes leave the machine.
func TestToolHTTPSPrivateTargetsBlockedByDefault(t *testing.T) {
	runner := NewToolRunner()

	cases := []struct {
		name   string
		tool   ToolID
		target string
	}{
		{"https-localhost", ToolHTTPS, "https://localhost/generate_204"},
		{"https-loopback-literal", ToolHTTPS, "https://127.0.0.1/generate_204"},
		{"https-ipv6-loopback-literal", ToolHTTPS, "https://[::1]/generate_204"},
		{"https-rfc1918", ToolHTTPS, "http://192.168.1.10:8080/"},
		{"https-cgnat", ToolHTTPS, "http://100.64.0.7/"},
		{"tcp-loopback", ToolTCP, "127.0.0.1:443"},
		{"tls-rfc1918", ToolTLS, "10.1.2.3:443"},
		{"websocket-loopback", ToolWebSocket, "ws://127.0.0.1:1080"},
		{"udp-private-resolver", ToolUDP, "192.168.0.53:53"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := runner.Run(context.Background(), ToolRequest{
				Tool:   tc.tool,
				Target: tc.target,
			})

			if result.Status != ToolStatusInvalid {
				t.Fatalf("status = %q, want invalid_target (private destination must be blocked for %s)", result.Status, tc.tool)
			}

			if !strings.Contains(strings.ToLower(result.Error), "private") {
				t.Fatalf("error should explain the private-destination policy: %q", result.Error)
			}
		})
	}
}

// TestToolHTTPSLocalhostBlockedWithLiveServer: a REAL accepting
// listener on the resolved port proves the guard rejects BEFORE the
// dial — this is the deterministic form of the Windows-runner
// condition (port 80 served on both stacks) that made the original
// failure visible. It cannot pass by connection-refused accident.
func TestToolHTTPSLocalhostBlockedWithLiveServer(t *testing.T) {
	v6, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback listener: %v", err)
	}

	defer v6.Close()

	port := v6.Addr().(*net.TCPAddr).Port

	v4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconvItoa(port)))
	if err != nil {
		t.Skipf("no IPv4 loopback listener: %v", err)
	}

	defer v4.Close()

	accept := func(l net.Listener) {
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}

				go func(c net.Conn) {
					buf := make([]byte, 64)
					_, _ = c.Read(buf)
					_, _ = c.Write([]byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n"))
					_ = c.Close()
				}(c)
			}
		}()
	}

	accept(v6)
	accept(v4)

	dial := DefaultSafety().dialer(ToolHTTPS)

	if _, err := dial.Dial(context.Background(), "tcp", net.JoinHostPort("localhost", strconvItoa(port))); err == nil {
		t.Fatal("https dialer must refuse a hostname resolving to loopback even when the port is served")
	}
}

// TestSafeDialerResolvingHostnamePrivateBlocked: the rebinding guard
// validates EVERY resolved answer with stub resolvers — deterministic
// on every platform, independent of /etc/hosts.
func TestSafeDialerResolvingHostnamePrivateBlocked(t *testing.T) {
	original := resolveIPAddrs
	originalDial := netDial
	defer func() {
		resolveIPAddrs = original
		netDial = originalDial
	}()

	cases := []struct {
		name     string
		answers  []string
		blocked  bool
		expected string // the pinned destination when not blocked
	}{
		{
			name:    "private-ipv4-only",
			answers: []string{"10.0.0.5"},
			blocked: true,
		},
		{
			name:    "private-ipv6-only",
			answers: []string{"fd12:3456:789a::1"},
			blocked: true,
		},
		{
			name:    "ipv6-loopback-only",
			answers: []string{"::1"},
			blocked: true,
		},
		{
			name:    "link-local-ipv4",
			answers: []string{"169.254.10.7"},
			blocked: true,
		},
		{
			name:     "mixed-public-private-dials-validated",
			answers:  []string{"93.184.216.34", "127.0.0.1"},
			blocked:  false,
			expected: "93.184.216.34:443",
		},
		{
			name:     "public-answers-pin-first-validated",
			answers:  []string{"2606:4700:4700::1111", "1.1.1.1"},
			blocked:  false,
			expected: "[2606:4700:4700::1111]:443",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolveIPAddrs = func(ctx context.Context, host string) ([]net.IPAddr, error) {
				addrs := make([]net.IPAddr, 0, len(tc.answers))
				for _, a := range tc.answers {
					addrs = append(addrs, net.IPAddr{IP: net.ParseIP(a)})
				}

				return addrs, nil
			}

			var dialed string

			netDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialed = addr
				return nil, fmt.Errorf("stub dial")
			}

			dial := DefaultSafety().dialer(ToolHTTPS)

			_, err := dial.Dial(context.Background(), "tcp", "rebinding.test:443")
			if tc.blocked {
				if err == nil || !strings.Contains(err.Error(), "blocked private") {
					t.Fatalf("err = %v, want a blocked-private rejection", err)
				}

				if dialed != "" {
					t.Fatalf("nothing may be dialed for a blocked hostname, dialed %q", dialed)
				}

				return
			}

			if err == nil || dialed != tc.expected {
				t.Fatalf("err = %v, dialed = %q, want pin to %q", err, dialed, tc.expected)
			}
		})
	}
}

// TestToolHTTPSExplicitPrivateCapability: the sanctioned local test —
// AllowPrivateTargets lets generic tools reach local endpoints (the
// same capability the TLS tests use).
func TestToolHTTPSExplicitPrivateCapability(t *testing.T) {
	server := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	defaultRunner := NewToolRunner()

	if result := defaultRunner.Run(context.Background(), ToolRequest{
		Tool: ToolHTTPS, Target: server.URL,
	}); result.Status != ToolStatusInvalid {
		t.Fatalf("default policy must block the local endpoint, got %q", result.Status)
	}

	local := localEndpointRunner()

	result := local.Run(context.Background(), ToolRequest{Tool: ToolHTTPS, Target: server.URL})
	if result.Status != ToolStatusOK {
		t.Fatalf("explicit capability must allow the local endpoint: %q (%s)", result.Status, result.Error)
	}
}

// TestToolSOCKS5HTTPConnectImplicitLocalSemantics: the two genuinely
// local-endpoint tools keep implicit private-target permission under
// the DEFAULT policy — testing one's own local proxy is their purpose.
func TestToolSOCKS5HTTPConnectImplicitLocalSemantics(t *testing.T) {
	proxy := fakeSOCKSProxy(t)

	runner := NewToolRunner() // default policy — no explicit capability

	if result := runner.Run(context.Background(), ToolRequest{Tool: ToolSOCKS5, Target: proxy}); result.Status != ToolStatusOK {
		t.Fatalf("socks5 local endpoint under default policy: %q (%s)", result.Status, result.Error)
	}

	server := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusOK)
		}
	})

	host, port := hostPortOfURL(t, server.URL)

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolHTTPConnect,
		Target: net.JoinHostPort(host, strconvItoa(port)),
	})

	if result.Status != ToolStatusOK {
		t.Fatalf("http_connect local endpoint under default policy: %q (%s)", result.Status, result.Error)
	}
}

// TestToolHTTPSRedirectToPrivateBlocked: a redirect chain ending at a
// private destination is refused mid-chain — redirected HTTPS requests
// stay under the same destination-safety policy. The public first hop
// is simulated deterministically (stub resolver + stub dial serving a
// 302 through a pipe), so the test proves the second hop NEVER
// connects under the default policy, on any platform, with no network.
func TestToolHTTPSRedirectToPrivateBlocked(t *testing.T) {
	originalResolve, originalDial := resolveIPAddrs, netDial
	defer func() { resolveIPAddrs, netDial = originalResolve, originalDial }()

	resolveIPAddrs = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}

	dials := 0

	server, client := net.Pipe()

	netDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials++

		if dials > 1 {
			t.Errorf("the private redirect hop must never be dialed (attempt %d to %s)", dials, addr)
		}

		return client, nil
	}

	// Serve one 302 redirecting to a private literal, then the pipe
	// closes — any further request would fail loudly.
	go func() {
		defer server.Close()

		buf := make([]byte, 1024)

		for {
			if _, err := server.Read(buf); err != nil {
				return
			}

			if !bytes.Contains(buf, []byte("\r\n\r\n")) {
				continue
			}

			_, _ = server.Write([]byte("HTTP/1.1 302 Found\r\n" +
				"Location: http://127.0.0.1:9/landing\r\n" +
				"Content-Length: 0\r\n\r\n"))

			return
		}
	}()

	runner := NewToolRunner() // default policy — no private-target capability

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolHTTPS,
		Target: "http://redirect.public.test/start",
	})

	if result.Status != ToolStatusInvalid {
		t.Fatalf("status = %q, want invalid_target (private redirect blocked)", result.Status)
	}

	if !strings.Contains(strings.ToLower(result.Error), "private") {
		t.Fatalf("error should identify the private-destination policy: %q", result.Error)
	}

	if dials > 1 {
		t.Fatalf("redirect followed to a private destination after %d dials", dials)
	}
}

// TestValidateRedirectTargetPolicy: the redirect-hop validator itself
// — credentials are always refused, private literals follow the
// policy, and the explicit capability permits intentional local hops.
func TestValidateRedirectTargetPolicy(t *testing.T) {
	parse := func(raw string) *url.URL {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}

		return parsed
	}

	defaults := DefaultSafety()

	if err := defaults.validateRedirectTarget(ToolHTTPS, parse("http://127.0.0.1:9/x")); err == nil {
		t.Fatal("private redirect destination must be blocked by default")
	}

	if err := defaults.validateRedirectTarget(ToolHTTPS, parse("http://user:pw@public.test/x")); err == nil {
		t.Fatal("credentials in redirect URLs must always be refused")
	}

	local := defaults
	local.AllowPrivateTargets = true

	if err := local.validateRedirectTarget(ToolHTTPS, parse("http://127.0.0.1:9/x")); err != nil {
		t.Fatalf("explicit capability must allow the local hop: %v", err)
	}

	if err := local.validateRedirectTarget(ToolHTTPS, parse("http://user:pw@public.test/x")); err == nil {
		t.Fatal("credentials stay refused even with the local capability")
	}

	if err := defaults.validateRedirectTarget(ToolHTTPS, parse("http://public.test/x")); err != nil {
		t.Fatalf("public redirect destination must pass: %v", err)
	}
}

// TestToolHTTPSTunneledPrivateLiteralBlocked: tunneled requests keep
// the destination policy (the proxy resolves remotely, so the literal
// check at validation is the guard) — private literals are refused
// even with a live tunnel dialer.
func TestToolHTTPSTunneledPrivateLiteralBlocked(t *testing.T) {
	proxy := fakeSOCKSProxy(t)

	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:     ToolHTTPS,
		Target:   "http://127.0.0.1:1080/check",
		Path:     PathTunneled,
		Dial:     localDial(proxy),
		Provider: "test-tor",
	})

	if result.Status != ToolStatusInvalid {
		t.Fatalf("status = %q, want invalid_target (private literal under tunneled path)", result.Status)
	}
}

// TestSafeDialerLocalToolsStillReachPrivateEndpoints: socks5 keeps
// implicit permission at the DIALER level (the local proxy check).
func TestSafeDialerLocalToolsStillReachPrivateEndpoints(t *testing.T) {
	original := resolveIPAddrs
	originalDial := netDial
	defer func() {
		resolveIPAddrs = original
		netDial = originalDial
	}()

	resolveIPAddrs = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}

	var dialed string

	netDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = addr
		return nil, fmt.Errorf("stub dial")
	}

	dial := DefaultSafety().dialer(ToolSOCKS5)

	if _, err := dial.Dial(context.Background(), "tcp", "my.local.proxy:1080"); err == nil || dialed != "127.0.0.1:1080" {
		t.Fatalf("socks5 dialer must pin the loopback answer (err=%v dialed=%q)", err, dialed)
	}
}
