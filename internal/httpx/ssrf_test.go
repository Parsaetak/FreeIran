package httpx

// v0.9.7 tests (§11/§23): SSRF protection — scheme allowlist, private
// range rejection (literal + hostname), port validation, redirect
// caps. DNS-rebinding prevention is enforced at dial time and covered
// by the Control hook unit checks.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateGlobalURLRejectsPrivateDestinations(t *testing.T) {
	cases := []struct {
		url    string
		reason string
	}{
		{"http://127.0.0.1/sub", "loopback literal"},
		{"http://10.1.2.3/sub", "RFC1918 10/8"},
		{"https://192.168.1.10/config.txt", "RFC1918 192.168/16"},
		{"https://172.20.0.5/sub", "RFC1918 172.16/12"},
		{"http://169.254.169.254/latest/meta-data", "cloud metadata"},
		{"http://[::1]/sub", "IPv6 loopback"},
		{"http://[fe80::1]/sub", "IPv6 link-local"},
		{"http://[fc00::1]/sub", "IPv6 unique-local"},
		{"http://localhost/sub", "localhost name"},
		{"http://metadata.google.internal/computeMetadata", "metadata host"},
		{"http://node.local/sub", ".local name"},
		{"http://service.internal/sub", ".internal name"},
		{"https://0.0.0.0/", "unspecified address"},
	}

	for _, tc := range cases {
		err := ValidateGlobalURL(tc.url, SSRFOptions{})
		if err == nil {
			t.Errorf("%s (%s): expected rejection, got nil", tc.url, tc.reason)
		}
	}
}

func TestValidateGlobalURLAcceptsPublicDestinations(t *testing.T) {
	cases := []string{
		"https://example.com/config.txt",
		"https://raw.githubusercontent.com/owner/repo/main/sub.txt",
		"http://example.com/nodes",
		"https://8.8.8.8/dns-query",
		"https://gist.github.com",
	}

	for _, tc := range cases {
		if err := ValidateGlobalURL(tc, SSRFOptions{}); err != nil {
			t.Errorf("%s: expected accept, got %v", tc, err)
		}
	}
}

func TestValidateGlobalURLRejectsBadSchemesAndPorts(t *testing.T) {
	cases := []struct {
		url    string
		reason string
	}{
		{"file:///etc/passwd", "file scheme"},
		{"gopher://example.com/", "gopher scheme"},
		{"ftp://example.com/file", "ftp scheme"},
		{"https://example.com:3306/sub", "mysql port"},
		{"https://example.com:6379/", "redis port"},
		{"https://example.com:99999/", "port out of range"},
	}

	for _, tc := range cases {
		err := ValidateGlobalURL(tc.url, SSRFOptions{})
		if err == nil {
			t.Errorf("%s (%s): expected rejection", tc.url, tc.reason)
		}
	}

	// AllowPrivate still refuses infrastructure ports...
	if err := ValidateGlobalURL("https://example.com:3306/sub", SSRFOptions{AllowPrivate: true}); err == nil {
		t.Error("mysql port must be refused even with AllowPrivate")
	}
}

func TestValidateGlobalURLHostPin(t *testing.T) {
	opts := SSRFOptions{AllowedHosts: []string{"raw.githubusercontent.com"}}

	if err := ValidateGlobalURL("https://raw.githubusercontent.com/o/r/main/s.txt", opts); err != nil {
		t.Fatalf("pinned host must pass: %v", err)
	}

	if err := ValidateGlobalURL("https://evil.example.com/sub", opts); err == nil {
		t.Fatal("non-pinned host must fail")
	}
}

func TestSSRFClientRefusesPrivateBeforeDialing(t *testing.T) {
	client := NewSSRFClient(Policy{RequestTimeout: time.Second}, SSRFOptions{})

	// Loopback URL: even a live local httptest server must never be
	// reached — the pre-flight rejects before any network activity.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("SSRF client reached a loopback server")
		_, _ = w.Write([]byte("leak"))
	}))
	defer server.Close()

	_, err := client.Get(context.Background(), server.URL, GetOptions{})
	if err == nil {
		t.Fatal("loopback fetch must fail")
	}

	if !errors.Is(err, ErrSSFRestricted) && !errors.Is(err, ErrSSFScheme) &&
		!strings.Contains(err.Error(), "not a public internet address") &&
		!strings.Contains(err.Error(), "only http/https") {
		// httptest may serve on 127.0.0.1 (restricted) — error family
		// already validated above; anything else is a real failure.
		t.Fatalf("unexpected error class: %v", err)
	}
}

func TestSSRFClientFollowsBoundedRedirects(t *testing.T) {
	mux := http.NewServeMux()

	// Endless redirect loop: every response is a redirect, so the
	// client must trip the cap regardless of retry attempts.
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hop", http.StatusFound)
	})
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hop", http.StatusFound)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	// Extract the random test port so the allowlist admits it.
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	portNumber := 0
	for _, r := range port {
		portNumber = portNumber*10 + int(r-'0')
	}

	client := NewSSRFClient(Policy{RequestTimeout: 2 * time.Second}, SSRFOptions{
		AllowPrivate: true, // loopback httptest
		MaxRedirects: 1,    // stricter than the loop's needs
		AllowedPorts: []int{portNumber},
	})

	_, err := client.Get(context.Background(), server.URL+"/start", GetOptions{})
	if err == nil {
		t.Fatal("redirect chain beyond the cap must fail")
	}

	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("expected redirect error, got %v", err)
	}
}

func TestIsPrivateIPTable(t *testing.T) {
	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "93.184.216.34"}

	for _, s := range public {
		if isPrivateIP(net.ParseIP(s)) {
			t.Errorf("%s must be public", s)
		}
	}

	private := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1",
		"169.254.1.1", "100.64.0.1", "224.0.0.1", "::1", "fe80::1", "fc00::1",
	}

	for _, s := range private {
		if !isPrivateIP(net.ParseIP(s)) {
			t.Errorf("%s must be private", s)
		}
	}
}
