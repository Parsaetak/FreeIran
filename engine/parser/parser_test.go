package parser

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func TestParseVLESS(t *testing.T) {
	p := New()

	input := "vless://test-uuid@example.com:443" +
		"?type=ws" +
		"&security=tls" +
		"&sni=example.com" +
		"&host=example.com" +
		"&path=%2Fvpn" +
		"#Test%20Server"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeVLESS {
		t.Fatalf("expected VLESS, got %q", cfg.Type)
	}

	if cfg.Address != "example.com" {
		t.Fatalf("unexpected address: %q", cfg.Address)
	}

	if cfg.Port != 443 {
		t.Fatalf("unexpected port: %d", cfg.Port)
	}

	if cfg.UUID != "test-uuid" {
		t.Fatalf("unexpected UUID: %q", cfg.UUID)
	}

	if cfg.Network != "ws" {
		t.Fatalf("unexpected network: %q", cfg.Network)
	}

	if cfg.Security != "tls" {
		t.Fatalf("unexpected security: %q", cfg.Security)
	}

	if cfg.ServerName != "example.com" {
		t.Fatalf("unexpected SNI: %q", cfg.ServerName)
	}

	if cfg.Host != "example.com" {
		t.Fatalf("unexpected host: %q", cfg.Host)
	}

	if cfg.Path != "/vpn" {
		t.Fatalf("unexpected path: %q", cfg.Path)
	}

	if cfg.Name != "Test Server" {
		t.Fatalf("unexpected name: %q", cfg.Name)
	}

	if cfg.ID == "" {
		t.Fatal("expected configuration ID")
	}
}

func TestParseVMess(t *testing.T) {
	p := New()

	payload := `{
		"ps":"Test VMess",
		"add":"vmess.example.com",
		"port":443,
		"id":"test-uuid",
		"aid":0,
		"net":"ws",
		"type":"none",
		"host":"vmess.example.com",
		"path":"/ws",
		"tls":"tls",
		"sni":"vmess.example.com"
	}`

	encoded := base64.RawStdEncoding.EncodeToString([]byte(payload))
	input := "vmess://" + encoded

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeVMess {
		t.Fatalf("expected VMess, got %q", cfg.Type)
	}

	if cfg.Address != "vmess.example.com" {
		t.Fatalf("unexpected address: %q", cfg.Address)
	}

	if cfg.Port != 443 {
		t.Fatalf("unexpected port: %d", cfg.Port)
	}

	if cfg.UUID != "test-uuid" {
		t.Fatalf("unexpected UUID: %q", cfg.UUID)
	}

	if cfg.Network != "ws" {
		t.Fatalf("unexpected network: %q", cfg.Network)
	}

	if cfg.Path != "/ws" {
		t.Fatalf("unexpected path: %q", cfg.Path)
	}

	if cfg.Security != "tls" {
		t.Fatalf("unexpected security: %q", cfg.Security)
	}

	if cfg.ServerName != "vmess.example.com" {
		t.Fatalf("unexpected SNI: %q", cfg.ServerName)
	}

	if cfg.Name != "Test VMess" {
		t.Fatalf("unexpected name: %q", cfg.Name)
	}
}

func TestParseTrojan(t *testing.T) {
	p := New()

	input := "trojan://test-password@example.com:443" +
		"?security=tls" +
		"&sni=example.com" +
		"#Trojan%20Server"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeTrojan {
		t.Fatalf("expected Trojan, got %q", cfg.Type)
	}

	if cfg.Password != "test-password" {
		t.Fatalf("unexpected password: %q", cfg.Password)
	}

	if cfg.Address != "example.com" {
		t.Fatalf("unexpected address: %q", cfg.Address)
	}

	if cfg.Port != 443 {
		t.Fatalf("unexpected port: %d", cfg.Port)
	}

	if cfg.Security != "tls" {
		t.Fatalf("unexpected security: %q", cfg.Security)
	}

	if cfg.ServerName != "example.com" {
		t.Fatalf("unexpected SNI: %q", cfg.ServerName)
	}

	if cfg.Name != "Trojan Server" {
		t.Fatalf("unexpected name: %q", cfg.Name)
	}
}

func TestParseShadowsocks(t *testing.T) {
	p := New()

	input := "ss://aes-256-gcm:test-password@example.com:8388#SS%20Server"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeShadowsocks {
		t.Fatalf("expected Shadowsocks, got %q", cfg.Type)
	}

	if cfg.Method != "aes-256-gcm" {
		t.Fatalf("unexpected method: %q", cfg.Method)
	}

	if cfg.Password != "test-password" {
		t.Fatalf("unexpected password: %q", cfg.Password)
	}

	if cfg.Address != "example.com" {
		t.Fatalf("unexpected address: %q", cfg.Address)
	}

	if cfg.Port != 8388 {
		t.Fatalf("unexpected port: %d", cfg.Port)
	}

	if cfg.Name != "SS Server" {
		t.Fatalf("unexpected name: %q", cfg.Name)
	}
}

func TestParseMultipleConfigurations(t *testing.T) {
	p := New()

	input := strings.Join([]string{
		"vless://uuid1@example.com:443",
		"trojan://password@example.org:443",
	}, "\n")

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 configurations, got %d", len(configs))
	}

	if configs[0].Type != config.TypeVLESS {
		t.Fatalf("expected first configuration to be VLESS")
	}

	if configs[1].Type != config.TypeTrojan {
		t.Fatalf("expected second configuration to be Trojan")
	}
}

func TestParseBase64Subscription(t *testing.T) {
	p := New()

	raw := strings.Join([]string{
		"vless://uuid1@example.com:443",
		"trojan://password@example.org:443",
	}, "\n")

	encoded := base64.StdEncoding.EncodeToString([]byte(raw))

	configs, err := p.Parse([]byte(encoded))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 configurations, got %d", len(configs))
	}
}

func TestParseDeduplicatesConfigurations(t *testing.T) {
	p := New()

	input := strings.Join([]string{
		"vless://uuid@example.com:443",
		"vless://uuid@example.com:443",
		"vless://uuid@example.com:443#DifferentName",
	}, "\n")

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf(
			"expected 1 configuration after deduplication, got %d",
			len(configs),
		)
	}
}

func TestParseRejectsUnsupportedScheme(t *testing.T) {
	p := New()

	_, err := p.Parse([]byte("ftp://example.com:443"))

	if err == nil {
		t.Fatal("expected unsupported scheme error")
	}

	if !strings.Contains(err.Error(), "unsupported configuration scheme") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseRejectsEmptyInput(t *testing.T) {
	p := New()

	_, err := p.Parse([]byte("   \n\t  "))

	if err == nil {
		t.Fatal("expected empty input error")
	}

	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseSkipsInvalidEntriesWhenValidEntriesExist(t *testing.T) {
	p := New()

	input := strings.Join([]string{
		"vless://uuid@example.com:443",
		"not-a-valid-config",
		"trojan://password@example.org:443",
	}, "\n")

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() should retain valid entries: %v", err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 valid configurations, got %d", len(configs))
	}
}

func TestParseFailsWhenNothingIsValid(t *testing.T) {
	p := New()

	input := strings.Join([]string{
		"not-a-valid-config",
		"ftp://example.com:443",
	}, "\n")

	_, err := p.Parse([]byte(input))

	if err == nil {
		t.Fatal("expected parsing to fail")
	}

	if !strings.Contains(err.Error(), "no valid configurations") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseNormalizesConfigurations(t *testing.T) {
	p := New()

	input := "VLESS://UUID@EXAMPLE.COM:443?type=WS&security=TLS"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeVLESS {
		t.Fatalf("expected normalized VLESS type, got %q", cfg.Type)
	}

	if cfg.Address != "example.com" {
		t.Fatalf("expected normalized address, got %q", cfg.Address)
	}

	if cfg.Network != "ws" {
		t.Fatalf("expected normalized network, got %q", cfg.Network)
	}

	if cfg.Security != "tls" {
		t.Fatalf("expected normalized security, got %q", cfg.Security)
	}
}
func TestParseHysteria2(t *testing.T) {
	p := New()

	input := "hysteria2://test-password@example.com:443" +
		"?sni=example.com" +
		"&insecure=1" +
		"&obfs=salamander" +
		"&obfs-password=obfs-secret" +
		"#HY2%20Server"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeHysteria2 {
		t.Fatalf("expected Hysteria2, got %q", cfg.Type)
	}

	if cfg.Address != "example.com" {
		t.Fatalf("unexpected address: %q", cfg.Address)
	}

	if cfg.Port != 443 {
		t.Fatalf("unexpected port: %d", cfg.Port)
	}

	if cfg.Password != "test-password" {
		t.Fatalf("unexpected password: %q", cfg.Password)
	}

	if cfg.ServerName != "example.com" {
		t.Fatalf("unexpected SNI: %q", cfg.ServerName)
	}

	if cfg.Name != "HY2 Server" {
		t.Fatalf("unexpected name: %q", cfg.Name)
	}
}

func TestParseHysteria2Alias(t *testing.T) {
	p := New()

	input := "hy2://test-password@example.com:8443?sni=example.com"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	if configs[0].Type != config.TypeHysteria2 {
		t.Fatalf(
			"expected Hysteria2, got %q",
			configs[0].Type,
		)
	}
}

func TestParseTUIC(t *testing.T) {
	p := New()

	input := "tuic://test-uuid:test-password@example.com:443" +
		"?sni=example.com" +
		"&congestion_control=bbr" +
		"#TUIC%20Server"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeTUIC {
		t.Fatalf("expected TUIC, got %q", cfg.Type)
	}

	if cfg.Address != "example.com" {
		t.Fatalf("unexpected address: %q", cfg.Address)
	}

	if cfg.Port != 443 {
		t.Fatalf("unexpected port: %d", cfg.Port)
	}

	if cfg.UUID != "test-uuid" {
		t.Fatalf("unexpected UUID: %q", cfg.UUID)
	}

	if cfg.Password != "test-password" {
		t.Fatalf("unexpected password: %q", cfg.Password)
	}

	if cfg.ServerName != "example.com" {
		t.Fatalf("unexpected SNI: %q", cfg.ServerName)
	}

	if cfg.Name != "TUIC Server" {
		t.Fatalf("unexpected name: %q", cfg.Name)
	}
}

func TestParseSOCKS(t *testing.T) {
	p := New()

	input := "socks5://username:password@example.com:1080#SOCKS%20Server"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeSOCKS {
		t.Fatalf("expected SOCKS, got %q", cfg.Type)
	}

	if cfg.Address != "example.com" {
		t.Fatalf("unexpected address: %q", cfg.Address)
	}

	if cfg.Port != 1080 {
		t.Fatalf("unexpected port: %d", cfg.Port)
	}

	if cfg.Username != "username" {
		t.Fatalf("unexpected username: %q", cfg.Username)
	}

	if cfg.Password != "password" {
		t.Fatalf("unexpected password: %q", cfg.Password)
	}

	if cfg.Name != "SOCKS Server" {
		t.Fatalf("unexpected name: %q", cfg.Name)
	}
}

func TestParseHTTPProxy(t *testing.T) {
	p := New()

	input := "http://username:password@example.com:8080#HTTP%20Proxy"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeHTTP {
		t.Fatalf("expected HTTP, got %q", cfg.Type)
	}

	if cfg.Address != "example.com" {
		t.Fatalf("unexpected address: %q", cfg.Address)
	}

	if cfg.Port != 8080 {
		t.Fatalf("unexpected port: %d", cfg.Port)
	}

	if cfg.Username != "username" {
		t.Fatalf("unexpected username: %q", cfg.Username)
	}

	if cfg.Password != "password" {
		t.Fatalf("unexpected password: %q", cfg.Password)
	}

	if cfg.Name != "HTTP Proxy" {
		t.Fatalf("unexpected name: %q", cfg.Name)
	}
}

func TestParseSOCKS4(t *testing.T) {
	p := New()

	input := "socks4://username@example.com:1080"

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeSOCKS {
		t.Fatalf("expected SOCKS, got %q", cfg.Type)
	}

	if cfg.Username != "username" {
		t.Fatalf("unexpected username: %q", cfg.Username)
	}
}

func TestParseProtocolMix(t *testing.T) {
	p := New()

	input := strings.Join([]string{
		"vless://uuid@example.com:443",
		"vmess://" + base64.RawStdEncoding.EncodeToString([]byte(`{
			"ps":"VMess",
			"add":"vmess.example.com",
			"port":443,
			"id":"vmess-uuid",
			"net":"ws",
			"path":"/ws",
			"tls":"tls"
		}`)),
		"trojan://trojan-password@trojan.example.com:443",
		"ss://aes-256-gcm:ss-password@ss.example.com:8388",
		"hysteria2://hy2-password@hy2.example.com:443",
		"tuic://tuic-uuid:tuic-password@tuic.example.com:443",
		"socks5://user:password@socks.example.com:1080",
		"http://user:password@http.example.com:8080",
	}, "\n")

	configs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(configs) != 8 {
		t.Fatalf(
			"expected 8 configurations, got %d",
			len(configs),
		)
	}

	expected := []config.Type{
		config.TypeVLESS,
		config.TypeVMess,
		config.TypeTrojan,
		config.TypeShadowsocks,
		config.TypeHysteria2,
		config.TypeTUIC,
		config.TypeSOCKS,
		config.TypeHTTP,
	}

	for i, expectedType := range expected {
		if configs[i].Type != expectedType {
			t.Fatalf(
				"configuration %d: expected %q, got %q",
				i,
				expectedType,
				configs[i].Type,
			)
		}
	}
}
