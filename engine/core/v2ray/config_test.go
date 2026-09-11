package v2ray_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
)

// v4Doc mirrors the V4 document shape for structural assertions.
type v4Doc struct {
	Log *struct {
		LogLevel string `json:"loglevel"`
	} `json:"log"`
	Inbounds []struct {
		Tag      string  `json:"tag"`
		Listen   string  `json:"listen"`
		Port     float64 `json:"port"`
		Protocol string  `json:"protocol"`
		Settings struct {
			Auth string `json:"auth"`
			UDP  bool   `json:"udp"`
		} `json:"settings"`
	} `json:"inbounds"`
	Outbounds []struct {
		Tag            string          `json:"tag"`
		Protocol       string          `json:"protocol"`
		Settings       json.RawMessage `json:"settings"`
		StreamSettings *struct {
			Network         string          `json:"network"`
			Security        string          `json:"security"`
			TLSSettings     json.RawMessage `json:"tlsSettings"`
			RealitySettings json.RawMessage `json:"realitySettings"`
			WSSettings      *struct {
				Path    string            `json:"path"`
				Headers map[string]string `json:"headers"`
			} `json:"wsSettings"`
			GRPCSettings *struct {
				ServiceName string `json:"serviceName"`
			} `json:"grpcSettings"`
			HTTPSettings json.RawMessage `json:"httpSettings"`
			QUICSettings json.RawMessage `json:"quicSettings"`
		} `json:"streamSettings"`
	} `json:"outbounds"`
	Routing *struct {
		Rules []struct {
			IP          []string `json:"ip"`
			OutboundTag string   `json:"outboundTag"`
		} `json:"rules"`
	} `json:"routing"`
}

// buildFor renders a document through the adapter with a fixed port.
func buildFor(t *testing.T, cfg config.Config) (v4Doc, core.RuntimeConfig) {
	t.Helper()

	backend := v2ray.New()

	doc, err := backend.BuildConfig(cfg, core.RuntimeOptions{
		LocalPort:       45090,
		DisableGenCache: true,
	})
	if err != nil {
		t.Fatalf("BuildConfig() = %v", err)
	}

	var parsed v4Doc

	if err := json.Unmarshal(doc.Data, &parsed); err != nil {
		t.Fatalf("generated document is not valid JSON: %v", err)
	}

	return parsed, doc
}

// commonStructure asserts the shared document skeleton: local SOCKS
// inbound, proxy/direct/block outbounds, private-range routing.
func commonStructure(t *testing.T, parsed v4Doc) {
	t.Helper()

	if parsed.Log == nil || parsed.Log.LogLevel != "warning" {
		t.Fatalf("log level = %+v, want warning", parsed.Log)
	}

	if len(parsed.Inbounds) == 0 {
		t.Fatal("no inbounds")
	}

	socks := parsed.Inbounds[0]

	if socks.Protocol != "socks" || socks.Listen != "127.0.0.1" {
		t.Fatalf("first inbound = %+v, want local socks", socks)
	}

	if socks.Port != 45090 {
		t.Fatalf("socks port = %v, want 45090", socks.Port)
	}

	if socks.Settings.Auth != "noauth" || !socks.Settings.UDP {
		t.Fatalf("socks settings = %+v", socks.Settings)
	}

	if len(parsed.Outbounds) < 3 {
		t.Fatalf("outbound count = %d, want >= 3", len(parsed.Outbounds))
	}

	if parsed.Outbounds[0].Tag != "proxy" {
		t.Fatalf("first outbound tag = %s, want proxy", parsed.Outbounds[0].Tag)
	}

	tags := map[string]bool{}

	for _, out := range parsed.Outbounds {
		tags[out.Tag] = true
	}

	if !tags["direct"] || !tags["block"] {
		t.Fatalf("missing direct/block outbounds: %v", tags)
	}

	if parsed.Routing == nil || len(parsed.Routing.Rules) == 0 {
		t.Fatal("no routing rules")
	}

	if parsed.Routing.Rules[0].OutboundTag != "direct" {
		t.Fatalf("first routing rule = %+v, want direct", parsed.Routing.Rules[0])
	}
}

// TestVLESSConfig verifies the VLESS → V2Ray configuration adapter:
// vnext addressing, UUID user, "none" encryption, TLS stream.
func TestVLESSConfig(t *testing.T) {
	parsed, _ := buildFor(t, vlessTLSConfig())

	commonStructure(t, parsed)

	proxy := parsed.Outbounds[0]

	if proxy.Protocol != "vless" {
		t.Fatalf("protocol = %s, want vless", proxy.Protocol)
	}

	var settings struct {
		VNext []struct {
			Address string  `json:"address"`
			Port    float64 `json:"port"`
			Users   []struct {
				ID         string  `json:"id"`
				Encryption string  `json:"encryption"`
				Flow       string  `json:"flow"`
				Level      float64 `json:"level"`
			} `json:"users"`
		} `json:"vnext"`
	}

	if err := json.Unmarshal(proxy.Settings, &settings); err != nil {
		t.Fatalf("vless settings: %v", err)
	}

	if len(settings.VNext) != 1 {
		t.Fatalf("vnext entries = %d", len(settings.VNext))
	}

	entry := settings.VNext[0]

	if entry.Address != "vless-tls.example.org" || entry.Port != 443 {
		t.Fatalf("vnext = %+v", entry)
	}

	if len(entry.Users) != 1 {
		t.Fatalf("users = %d", len(entry.Users))
	}

	user := entry.Users[0]

	if user.ID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("uuid = %s", user.ID)
	}

	if user.Encryption != "none" {
		t.Fatalf("encryption = %s, want none", user.Encryption)
	}

	if user.Flow != "" {
		t.Fatalf("flow = %q, want empty on V2Ray", user.Flow)
	}

	if proxy.StreamSettings == nil {
		t.Fatal("no streamSettings")
	}

	stream := proxy.StreamSettings

	if stream.Network != "tcp" || stream.Security != "tls" {
		t.Fatalf("stream = %+v", stream)
	}

	var tls struct {
		ServerName string `json:"serverName"`
	}

	if err := json.Unmarshal(stream.TLSSettings, &tls); err != nil {
		t.Fatalf("tls settings: %v", err)
	}

	if tls.ServerName != "vless-tls.example.org" {
		t.Fatalf("serverName = %s", tls.ServerName)
	}

	if stream.RealitySettings != nil {
		t.Fatal("V2Ray document must never carry realitySettings")
	}
}

// TestVMessConfig verifies the VMess → V2Ray configuration adapter:
// vnext user with alterId 0 and "auto" security.
func TestVMessConfig(t *testing.T) {
	parsed, _ := buildFor(t, vmessGRPCConfig())

	commonStructure(t, parsed)

	proxy := parsed.Outbounds[0]

	if proxy.Protocol != "vmess" {
		t.Fatalf("protocol = %s, want vmess", proxy.Protocol)
	}

	var settings struct {
		VNext []struct {
			Users []struct {
				ID       string  `json:"id"`
				AlterID  float64 `json:"alterId"`
				Security string  `json:"security"`
			} `json:"users"`
		} `json:"vnext"`
	}

	if err := json.Unmarshal(proxy.Settings, &settings); err != nil {
		t.Fatalf("vmess settings: %v", err)
	}

	user := settings.VNext[0].Users[0]

	if user.ID != "33333333-3333-3333-3333-333333333333" {
		t.Fatalf("uuid = %s", user.ID)
	}

	if user.AlterID != 0 {
		t.Fatalf("alterId = %v, want 0", user.AlterID)
	}

	if user.Security != "auto" {
		t.Fatalf("security = %s, want auto", user.Security)
	}

	stream := proxy.StreamSettings

	if stream.Network != "grpc" || stream.Security != "tls" {
		t.Fatalf("stream = %+v", stream)
	}

	if stream.GRPCSettings == nil || stream.GRPCSettings.ServiceName != "grpcsvc" {
		t.Fatalf("grpc settings = %+v", stream.GRPCSettings)
	}
}

// TestTrojanConfig verifies the Trojan → V2Ray configuration adapter:
// servers password and TLS-by-default security.
func TestTrojanConfig(t *testing.T) {
	cfg := trojanConfig()
	cfg.Security = "" // trojan implies TLS

	parsed, _ := buildFor(t, cfg)

	commonStructure(t, parsed)

	proxy := parsed.Outbounds[0]

	if proxy.Protocol != "trojan" {
		t.Fatalf("protocol = %s, want trojan", proxy.Protocol)
	}

	var settings struct {
		Servers []struct {
			Address  string  `json:"address"`
			Port     float64 `json:"port"`
			Password string  `json:"password"`
		} `json:"servers"`
	}

	if err := json.Unmarshal(proxy.Settings, &settings); err != nil {
		t.Fatalf("trojan settings: %v", err)
	}

	server := settings.Servers[0]

	if server.Address != "trojan.example.org" || server.Port != 443 {
		t.Fatalf("server = %+v", server)
	}

	if server.Password != "synthetic-trojan-password" {
		t.Fatalf("password = %s", server.Password)
	}

	if proxy.StreamSettings.Security != "tls" {
		t.Fatalf("security = %s, want tls (trojan implies TLS)",
			proxy.StreamSettings.Security)
	}
}

// TestShadowsocksConfig verifies the Shadowsocks → V2Ray
// configuration adapter: method, password, no stream settings.
func TestShadowsocksConfig(t *testing.T) {
	parsed, _ := buildFor(t, shadowsocksConfig())

	commonStructure(t, parsed)

	proxy := parsed.Outbounds[0]

	if proxy.Protocol != "shadowsocks" {
		t.Fatalf("protocol = %s, want shadowsocks", proxy.Protocol)
	}

	var settings struct {
		Servers []struct {
			Address  string  `json:"address"`
			Port     float64 `json:"port"`
			Method   string  `json:"method"`
			Password string  `json:"password"`
		} `json:"servers"`
	}

	if err := json.Unmarshal(proxy.Settings, &settings); err != nil {
		t.Fatalf("shadowsocks settings: %v", err)
	}

	server := settings.Servers[0]

	if server.Method != "aes-256-gcm" {
		t.Fatalf("method = %s", server.Method)
	}

	if server.Password != "synthetic-ss-password" {
		t.Fatalf("password = %s", server.Password)
	}
}

// TestWebSocketTransport verifies ws path/host emission.
func TestWebSocketTransport(t *testing.T) {
	parsed, _ := buildFor(t, vlessWSConfig())

	stream := parsed.Outbounds[0].StreamSettings

	if stream.Network != "ws" {
		t.Fatalf("network = %s, want ws", stream.Network)
	}

	if stream.WSSettings == nil {
		t.Fatal("no wsSettings")
	}

	if stream.WSSettings.Path != "/tunnel" {
		t.Fatalf("ws path = %s", stream.WSSettings.Path)
	}

	if stream.WSSettings.Headers["Host"] != "vless-ws.example.org" {
		t.Fatalf("ws host header = %v", stream.WSSettings.Headers)
	}
}

// TestQUICAndH2Transports verifies the classic transports V2Ray
// retains (verified: Xray removed them).
func TestQUICAndH2Transports(t *testing.T) {
	quicParsed, _ := buildFor(t, vmessQUICConfig())

	if stream := quicParsed.Outbounds[0].StreamSettings; stream.Network != "quic" {
		t.Fatalf("network = %s, want quic", stream.Network)
	}

	h2Parsed, _ := buildFor(t, vlessH2Config())

	if stream := h2Parsed.Outbounds[0].StreamSettings; stream.Network != "http" {
		t.Fatalf("network = %s, want http (h2)", stream.Network)
	}
}

// TestRedactedSummaryNeverLeaks verifies the document summary and
// the configuration display never carry credentials.
func TestRedactedSummaryNeverLeaks(t *testing.T) {
	cases := []config.Config{
		vlessTLSConfig(),
		trojanConfig(),
		shadowsocksConfig(),
		socksConfig(),
	}

	backend := v2ray.New()

	for _, cfg := range cases {
		doc, err := backend.BuildConfig(cfg, core.RuntimeOptions{
			LocalPort:       45091,
			DisableGenCache: true,
		})
		if err != nil {
			t.Fatalf("BuildConfig(%s) = %v", cfg.Type, err)
		}

		for _, secret := range cfg.SecretFields() {
			if secret == "" {
				continue
			}

			if contains(doc.RedactedSummary, secret) {
				t.Fatalf("%s summary leaks credential", cfg.Type)
			}

			if contains(cfg.DisplayURL(), secret) {
				t.Fatalf("%s display URL leaks credential", cfg.Type)
			}

			if contains(backend.Capabilities().Summary(), secret) {
				t.Fatalf("%s capability summary leaks credential", cfg.Type)
			}
		}
	}
}

// TestValidateRejectsREALITY ensures the V2Ray adapter refuses
// REALITY configurations with a precise reason.
func TestValidateRejectsREALITY(t *testing.T) {
	backend := v2ray.New()

	err := backend.Validate(context.Background(), realityConfig())
	if err == nil {
		t.Fatal("Validate() should reject REALITY on V2Ray")
	}

	if !contains(err.Error(), "reality") {
		t.Fatalf("error should mention reality: %v", err)
	}

	// BuildConfig must refuse too.
	_, err = backend.BuildConfig(realityConfig(), core.RuntimeOptions{LocalPort: 45092})
	if err == nil {
		t.Fatal("BuildConfig() should reject REALITY on V2Ray")
	}
}

// TestGenCacheRoundTrip verifies the generation cache: identical
// input+options hit; changed options invalidate.
func TestGenCacheRoundTrip(t *testing.T) {
	backend := v2ray.New()

	opts := core.RuntimeOptions{LocalPort: 45093}

	first, err := backend.BuildConfig(vlessTLSConfig(), opts)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	second, err := backend.BuildConfig(vlessTLSConfig(), opts)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	if string(first.Data) != string(second.Data) {
		t.Fatal("cached document differs from regenerated document")
	}

	// Different port → different generation → different document.
	third, err := backend.BuildConfig(vlessTLSConfig(), core.RuntimeOptions{LocalPort: 45094})
	if err != nil {
		t.Fatalf("third build: %v", err)
	}

	if string(third.Data) == string(first.Data) {
		t.Fatal("port change must invalidate the generation cache")
	}
}

// contains is a tiny strings.Contains alias for test readability.
func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}

	return -1
}
