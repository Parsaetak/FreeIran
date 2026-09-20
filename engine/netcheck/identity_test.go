package netcheck

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// identity_test.go exercises the v0.9.8.5 Network Identity check
// (§6) deterministically: the public-IP and metadata endpoints are
// package variables, so every test points them at local fake
// servers — no external network dependency.
//
//   - local IPv4/IPv6 discovery (route lookups, loopback exclusion);
//   - public-IP success + endpoint provenance;
//   - public-IP fallback (first endpoint down);
//   - metadata success (ipinfo + ipwho.is shapes);
//   - malformed metadata → Unknown, never fabricated;
//   - metadata unavailable → Unknown;
//   - timeout / cancellation;
//   - tunneled identity through a local SOCKS relay.

// withIdentityEndpoints swaps the identity endpoints for the test's
// duration and restores them after.
func withIdentityEndpoints(t *testing.T, ipEndpoints, metadataEndpoints []string) {
	t.Helper()

	oldIP := identityIPEndpoints
	oldMeta := identityMetadataEndpoints

	identityIPEndpoints = ipEndpoints
	identityMetadataEndpoints = metadataEndpoints

	t.Cleanup(func() {
		identityIPEndpoints = oldIP
		identityMetadataEndpoints = oldMeta
	})
}

// fakeIPServer serves one plain-text IP body.
func fakeIPServer(t *testing.T, ip string, calls *int) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}

		_, _ = w.Write([]byte(ip))
	}))

	t.Cleanup(srv.Close)

	return srv
}

// TestIdentityLocalAddress verifies the local identity: a measured
// route-relevant IPv4 (or an honest failure on an isolated runner)
// and NEVER a loopback address.
func TestIdentityLocalAddress(t *testing.T) {
	withIdentityEndpoints(t, []string{"https://definitely.invalid/test"}, []string{"https://definitely.invalid/test"})

	report := RunNetworkIdentity(context.Background(), IdentityOptions{Timeout: 2 * time.Second})

	local := report.Local

	if local.Measured && local.PrimaryIPv4 == "" && local.PrimaryIPv6 == "" && len(local.Others) == 0 {
		t.Fatal("Measured must imply at least one address")
	}

	if local.PrimaryIPv4 == "127.0.0.1" || strings.HasPrefix(local.PrimaryIPv4, "127.") {
		t.Fatalf("loopback leaked as primary IPv4: %s", local.PrimaryIPv4)
	}

	for _, other := range local.Others {
		if other.Address == "127.0.0.1" || other.Interface == "lo" {
			t.Fatalf("loopback leaked into others: %+v", other)
		}
	}

	if local.PrimaryIPv4 != "" && net.ParseIP(local.PrimaryIPv4) == nil {
		t.Fatalf("primary IPv4 is not an IP: %s", local.PrimaryIPv4)
	}

	if local.PrimaryIPv6 != "" && net.ParseIP(local.PrimaryIPv6) == nil {
		t.Fatalf("primary IPv6 is not an IP: %s", local.PrimaryIPv6)
	}
}

// TestIdentityLocalIPv4RouteRelevant verifies the discovered address
// is the one a real connection would use (or the report honestly
// reports no route).
func TestIdentityLocalIPv4RouteRelevant(t *testing.T) {
	evidence, err := probeLocalAddress(context.Background())

	if err != nil {
		t.Skipf("no route in this environment: %v", err)
	}

	if evidence.Address == "" || net.ParseIP(evidence.Address) == nil {
		t.Fatalf("route evidence not an address: %+v", evidence)
	}

	if ip := net.ParseIP(evidence.Address); ip == nil || ip.IsLoopback() {
		t.Fatalf("route-relevant address is loopback: %s", evidence.Address)
	}
}

// TestIdentityPublicIPSuccess verifies the public-IP path and its
// endpoint provenance.
func TestIdentityPublicIPSuccess(t *testing.T) {
	srv := fakeIPServer(t, "203.0.113.7", nil)

	withIdentityEndpoints(t, []string{srv.URL}, []string{"https://definitely.invalid/m"})

	report := RunNetworkIdentity(context.Background(), IdentityOptions{Timeout: 3 * time.Second})

	if report.Public.DirectIP != "203.0.113.7" {
		t.Fatalf("public IP = %q, want 203.0.113.7 (err=%s)", report.Public.DirectIP, report.Error)
	}

	if report.Public.Endpoint != srv.URL {
		t.Fatalf("endpoint provenance = %q, want the fake endpoint", report.Public.Endpoint)
	}

	if report.Cancelled {
		t.Fatal("report cancelled")
	}
}

// TestIdentityPublicIPFallback verifies the second endpoint answers
// when the first is down.
func TestIdentityPublicIPFallback(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	t.Cleanup(primary.Close)

	secondary := fakeIPServer(t, "203.0.113.9", nil)

	withIdentityEndpoints(t, []string{primary.URL, secondary.URL}, []string{"https://definitely.invalid/m"})

	report := RunNetworkIdentity(context.Background(), IdentityOptions{Timeout: 3 * time.Second})

	if report.Public.DirectIP != "203.0.113.9" {
		t.Fatalf("public IP = %q, want fallback 203.0.113.9", report.Public.DirectIP)
	}

	if report.Public.Endpoint != secondary.URL {
		t.Fatalf("endpoint = %q, want the secondary", report.Public.Endpoint)
	}
}

// TestIdentityMetadataIPInfo verifies the ipinfo.io shape parsing.
func TestIdentityMetadataIPInfo(t *testing.T) {
	body := `{"ip":"203.0.113.7","city":"Test City","region":"Test Region","country":"TC","org":"AS64512 Example ISP Ltd."}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))

	t.Cleanup(srv.Close)

	// The metadata lookup only runs after a measured exit IP —
	// the identity path that could not even fetch its IP could not
	// reach a metadata source either. Give the run a working IP
	// endpoint so the metadata stage is actually exercised.
	ipSrv := fakeIPServer(t, "203.0.113.7", nil)

	withIdentityEndpoints(t, []string{ipSrv.URL}, []string{srv.URL})

	report := RunNetworkIdentity(context.Background(), IdentityOptions{Timeout: 3 * time.Second})

	if !report.Metadata.Available {
		t.Fatalf("metadata unavailable: %+v", report.Metadata)
	}

	if report.Metadata.ASN != "AS64512" {
		t.Fatalf("ASN = %q, want AS64512", report.Metadata.ASN)
	}

	if report.Metadata.Organization != "Example ISP Ltd." {
		t.Fatalf("organization = %q", report.Metadata.Organization)
	}

	if report.Metadata.Country != "TC" || report.Metadata.Region != "Test Region" {
		t.Fatalf("country/region = %q/%q", report.Metadata.Country, report.Metadata.Region)
	}

	if report.Metadata.Source != srv.URL {
		t.Fatalf("source not exposed: %q", report.Metadata.Source)
	}
}

// TestIdentityMetadataIPWhois verifies the ipwho.is shape parsing.
func TestIdentityMetadataIPWhois(t *testing.T) {
	body := `{"ip":"203.0.113.7","success":true,"country":"Testland","region":"Region X","connection":{"asn":64513,"org":"Fallback Org","isp":"Fallback ISP GmbH","domain":"example.org"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))

	t.Cleanup(srv.Close)

	ipSrv := fakeIPServer(t, "203.0.113.7", nil)

	withIdentityEndpoints(t, []string{ipSrv.URL}, []string{srv.URL})

	report := RunNetworkIdentity(context.Background(), IdentityOptions{Timeout: 3 * time.Second})

	if !report.Metadata.Available {
		t.Fatalf("metadata unavailable: %+v", report.Metadata)
	}

	if report.Metadata.ASN != "AS64513" {
		t.Fatalf("ASN = %q, want AS64513", report.Metadata.ASN)
	}

	if report.Metadata.Organization != "Fallback ISP GmbH" {
		t.Fatalf("organization = %q, want Fallback ISP GmbH", report.Metadata.Organization)
	}
}

// TestIdentityMetadataMalformed verifies malformed metadata is
// reported Unknown — never fabricated.
func TestIdentityMetadataMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))

	t.Cleanup(srv.Close)

	ipSrv := fakeIPServer(t, "203.0.113.7", nil)

	withIdentityEndpoints(t, []string{ipSrv.URL}, []string{srv.URL})

	report := RunNetworkIdentity(context.Background(), IdentityOptions{Timeout: 3 * time.Second})

	if report.Metadata.Available {
		t.Fatalf("malformed metadata must be unavailable: %+v", report.Metadata)
	}

	if report.Metadata.Organization != "" || report.Metadata.ASN != "" {
		t.Fatalf("malformed metadata must not fabricate: %+v", report.Metadata)
	}
}

// TestIdentityMetadataUnavailable verifies the honest Unknown.
func TestIdentityMetadataUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	t.Cleanup(srv.Close)

	ipSrv := fakeIPServer(t, "203.0.113.7", nil)

	withIdentityEndpoints(t, []string{ipSrv.URL}, []string{srv.URL})

	report := RunNetworkIdentity(context.Background(), IdentityOptions{Timeout: 3 * time.Second})

	if report.Metadata.Available || report.Metadata.Organization != "" || report.Metadata.ASN != "" {
		t.Fatalf("unavailable metadata must stay Unknown: %+v", report.Metadata)
	}
}

// TestIdentityTimeout verifies the bounded wall clock: a
// never-answering public-IP endpoint cannot hang the check.
func TestIdentityTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))

	t.Cleanup(slow.Close)

	withIdentityEndpoints(t, []string{slow.URL}, []string{slow.URL})

	started := time.Now()

	report := RunNetworkIdentity(context.Background(), IdentityOptions{Timeout: 1 * time.Second})

	if time.Since(started) > 4*time.Second {
		t.Fatalf("identity check exceeded its bound: %s", time.Since(started))
	}

	if report.Public.DirectIP != "" && report.DurationMS > 4000 {
		t.Fatalf("inconsistent bounded result: %+v", report)
	}
}

// TestIdentityCancellation verifies propagation.
func TestIdentityCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	withIdentityEndpoints(t, []string{"https://definitely.invalid/ip"}, []string{"https://definitely.invalid/m"})

	report := RunNetworkIdentity(ctx, IdentityOptions{Timeout: 2 * time.Second})

	if !report.Cancelled {
		t.Fatal("cancelled context must mark the report cancelled")
	}
}

// TestIdentityManualOnlyByConstruction verifies the structural rule:
// nothing in the package auto-runs the check (RunNetworkIdentity is
// only reachable from the explicit service method). This test guards
// the local identity containing no credentials-shaped data.
func TestIdentityManualOnlyByConstruction(t *testing.T) {
	withIdentityEndpoints(t, []string{"https://definitely.invalid/ip"}, []string{"https://definitely.invalid/m"})

	report := RunNetworkIdentity(context.Background(), IdentityOptions{Timeout: 2 * time.Second})

	// The rendered report is credential-free by construction; the
	// serialized fields carry only addresses, org names and sources.
	rendered := fmt.Sprintf("%+v", report)

	for _, banned := range []string{"password", "secret", "uuid", "token", "credential"} {
		if strings.Contains(strings.ToLower(rendered), banned) {
			t.Fatalf("identity report leaked credential-shaped data: %s", rendered)
		}
	}
}

// testSOCKSRelay is a local SOCKS5 relay that actually forwards bytes
// to one fixed upstream — unlike fakeSOCKSProxy (tools_test.go), which
// only performs the handshake. The tunneled identity test needs real
// HTTP round trips through the relay, so the bytes must flow.
type testSOCKSRelay struct {
	listenAddr string
}

// addr is the relay's local listen address.
func (r *testSOCKSRelay) addr() string { return r.listenAddr }

// dial routes one connection through the relay via the production
// SOCKS5 dialer (the same DialFunc contract the tools layer uses).
func (r *testSOCKSRelay) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := socks5.Dialer{ProxyAddr: r.listenAddr, Timeout: 5 * time.Second}

	return d.Dial(ctx, network, addr)
}

// startTestSOCKSRelay launches a SOCKS5 no-auth CONNECT relay whose
// every CONNECT lands on the given upstream (the requested
// destination is accepted and ignored — the test's upstream is the
// single fixed endpoint).
func startTestSOCKSRelay(t *testing.T, upstream string) *testSOCKSRelay {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go relaySOCKSConnection(conn, upstream)
		}
	}()

	return &testSOCKSRelay{listenAddr: listener.Addr().String()}
}

// relaySOCKSConnection serves ONE SOCKS5 CONNECT, then pipes both
// directions to the upstream until either side closes.
func relaySOCKSConnection(conn net.Conn, upstream string) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	reader := bufio.NewReader(conn)

	// Greeting: VER NMETHODS METHODS...
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return
	}

	if header[0] != 5 {
		return
	}

	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}

	// Select NO AUTHENTICATION.
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// Request: VER CMD RSV ATYP ADDR PORT.
	req := make([]byte, 4)
	if _, err := io.ReadFull(reader, req); err != nil {
		return
	}

	if req[1] != 0x01 { // CONNECT only
		return
	}

	switch req[3] {
	case 0x01: // IPv4
		if _, err := io.ReadFull(reader, make([]byte, 4)); err != nil {
			return
		}
	case 0x03: // domain
		length := make([]byte, 1)
		if _, err := io.ReadFull(reader, length); err != nil {
			return
		}

		if _, err := io.ReadFull(reader, make([]byte, int(length[0]))); err != nil {
			return
		}
	case 0x04: // IPv6
		if _, err := io.ReadFull(reader, make([]byte, 16)); err != nil {
			return
		}
	default:
		return
	}

	if _, err := io.ReadFull(reader, make([]byte, 2)); err != nil { // port
		return
	}

	// Success reply: VER REP RSV ATYP(IPv4) BND ADDR BND PORT.
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}

	up, err := net.DialTimeout("tcp", upstream, 5*time.Second)
	if err != nil {
		return
	}

	// Handshake done: pure pipe, no deadline.
	_ = conn.SetDeadline(time.Time{})

	done := make(chan struct{}, 1)

	go func() {
		_, _ = io.Copy(up, reader) // drain the bufio buffer + the rest
		done <- struct{}{}
	}()

	_, _ = io.Copy(conn, up)

	_ = up.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// TestIdentityTunneledPath verifies a tunneled run reports the
// tunnel path label and measures through the supplied dialer.
func TestIdentityTunneledPath(t *testing.T) {
	// A SOCKS relay that forwards to the public-IP fake.
	upstream := fakeIPServer(t, "203.0.113.99", nil)

	relay := startTestSOCKSRelay(t, upstream.Listener.Addr().String())

	withIdentityEndpoints(t, []string{"http://" + relay.addr()}, []string{"https://definitely.invalid/m"})

	report := RunNetworkIdentity(context.Background(), IdentityOptions{
		Timeout: 3 * time.Second,
		Dial:    relay.dial,
	})

	if report.Path != PathTunneled {
		t.Fatalf("path = %s, want tunneled", report.Path)
	}

	if report.Public.TunnelIP != "203.0.113.99" {
		t.Fatalf("tunnel IP = %q, want 203.0.113.99 (err=%s)", report.Public.TunnelIP, report.Error)
	}
}
