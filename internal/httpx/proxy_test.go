package httpx

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// proxy_test.go pins the v0.9.8.6 explicit-transport-policy contract:
//
//   - the default client NEVER inherits ambient HTTP_PROXY /
//     HTTPS_PROXY / ALL_PROXY variables (ProxyDirect);
//   - the SSRF-guarded discovery client is direct by policy: an
//     ambient proxy cannot bypass the dial-time destination
//     validation (proxy-assisted destination confusion);
//   - a user-configured proxy IS honoured when explicitly selected
//     (ProxyURL);
//   - unsupported/malformed explicit proxy configurations fail loudly
//     at construction.

// ambientProxyServer stands up a fake "attacker" proxy that records
// every connection attempt.
func ambientProxyServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))

	t.Cleanup(srv.Close)

	return srv, &hits
}

// withAmbientProxy points the standard proxy variables at the fake
// proxy for the duration of the test.
func withAmbientProxy(t *testing.T, proxyURL string) {
	t.Helper()

	t.Setenv("HTTP_PROXY", proxyURL)
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("ALL_PROXY", proxyURL)
	t.Setenv("http_proxy", proxyURL)
	t.Setenv("https_proxy", proxyURL)
	t.Setenv("all_proxy", proxyURL)
}

// TestDefaultClientIgnoresAmbientProxy proves the default production
// client is DIRECT: with every ambient proxy variable pointing at a
// recording proxy, a GET still reaches the real target and the proxy
// sees zero connections.
func TestDefaultClientIgnoresAmbientProxy(t *testing.T) {
	proxy, hits := ambientProxyServer(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("real-target"))
	}))
	t.Cleanup(target.Close)

	withAmbientProxy(t, proxy.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := Default().Get(ctx, target.URL, GetOptions{})
	if err != nil {
		t.Fatalf("GET with ambient proxy set: %v (ambient proxy must be ignored)", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the real target", resp.StatusCode)
	}

	if got := hits.Load(); got != 0 {
		t.Fatalf("ambient proxy received %d connections; the default client must be direct", got)
	}
}

// TestSSRFClientIgnoresAmbientProxy proves the security-sensitive
// discovery client is direct by POLICY: an ambient proxy that would
// happily serve internal addresses never receives a connection, and
// the request reaches the real destination (whose IP the dial-time
// Control hook validated).
func TestSSRFClientIgnoresAmbientProxy(t *testing.T) {
	proxy, hits := ambientProxyServer(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	withAmbientProxy(t, proxy.URL)

	// Allow the ephemeral httptest port through the SSRF port policy
	// (the guard's default set is 80/443).
	_, targetPort, err := net.SplitHostPort(target.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	port, err := strconv.Atoi(targetPort)
	if err != nil {
		t.Fatal(err)
	}

	client := NewSSRFClient(Policy{RequestTimeout: 5 * time.Second}, SSRFOptions{
		// The loopback target is allowed for this test explicitly.
		AllowPrivate: true,
		AllowedPorts: []int{port},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.Get(ctx, target.URL, GetOptions{}); err != nil {
		t.Fatalf("SSRF GET with ambient proxy set: %v (ambient proxy must be ignored)", err)
	}

	if got := hits.Load(); got != 0 {
		t.Fatalf("ambient proxy received %d connections; the SSRF client must be direct", got)
	}
}

// TestExplicitUserProxyIsHonoured proves the user-configured proxy
// dimension: selecting ProxyURL routes the request THROUGH that proxy
// (the proxy handler observes the absolute-URI request).
func TestExplicitUserProxyIsHonoured(t *testing.T) {
	var proxied atomic.Int32

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)

		// A forward proxy sees the absolute URI; answer as the origin.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("via-proxy"))
	}))
	t.Cleanup(proxy.Close)

	client := NewClient(Policy{
		RequestTimeout: 5 * time.Second,
		MaxRetries:     0,
		Proxy:          ProxySpec{Mode: ProxyURL, URL: proxy.URL},
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Any URL works: the proxy intercepts it regardless of resolvability.
	resp, err := client.Get(ctx, "http://example.test/asset", GetOptions{})
	if err != nil {
		t.Fatalf("GET through the explicit user proxy: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the proxy", resp.StatusCode)
	}

	if got := proxied.Load(); got == 0 {
		t.Fatal("the explicit user proxy was never contacted")
	}
}

// TestProxySpecValidation pins the loud-failure contract of invalid
// explicit proxy configurations.
func TestProxySpecValidation(t *testing.T) {
	if _, err := (ProxySpec{Mode: ProxyURL}).ProxyFunc(); err == nil {
		t.Fatal("ProxyURL without a URL must fail")
	}

	if _, err := (ProxySpec{Mode: ProxyURL, URL: "socks5://127.0.0.1:1080"}).ProxyFunc(); err == nil {
		t.Fatal("SOCKS proxy URLs must be rejected (dial-layer tunneling only)")
	}

	if _, err := (ProxySpec{Mode: ProxyURL, URL: "http://127.0.0.1:8080"}).ProxyFunc(); err != nil {
		t.Fatalf("http proxy URL must be accepted: %v", err)
	}

	if _, err := (ProxySpec{Mode: ProxyDirect}).ProxyFunc(); err != nil {
		t.Fatalf("direct must never fail: %v", err)
	}

	if fn, err := (ProxySpec{Mode: ProxyDirect}).ProxyFunc(); err != nil || fn != nil {
		t.Fatalf("direct must produce a nil Proxy func (fn=%v err=%v)", fn != nil, err)
	}
}

// TestAmbientProxyDoesNotLeakAcrossDefaultClientDouble-checks the
// environment is restored: a SECOND default-client request after the
// ambient variables were unset still works (guards test pollution).
func TestDefaultClientWorksWithoutAmbientProxy(t *testing.T) {
	os.Unsetenv("HTTP_PROXY")
	os.Unsetenv("HTTPS_PROXY")
	os.Unsetenv("ALL_PROXY")

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := Default().Get(ctx, target.URL, GetOptions{}); err != nil {
		t.Fatalf("plain GET failed: %v", err)
	}
}
