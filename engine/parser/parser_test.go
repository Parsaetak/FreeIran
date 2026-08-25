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
		t.Fatalf("expected address example.com, got %q", cfg.Address)
	}

	if cfg.Port != 443 {
		t.Fatalf("expected port 443, got %d", cfg.Port)
	}

	if cfg.UUID != "test-uuid" {
		t.Fatalf("expected UUID test-uuid, got %q", cfg.UUID)
	}

	if cfg.Network != "ws" {
		t.Fatalf("expected network ws, got %q", cfg.Network)
	}

	if cfg.Security != "tls" {
		t.Fatalf("expected security tls, got %q", cfg.Security)
	}

	if cfg.ServerName != "example.com" {
		t.Fatalf("expected SNI example.com, got %q", cfg.ServerName)
	}

	if cfg.Path != "/vpn" {
		t.Fatalf("expected path /vpn, got %q", cfg.Path)
	}

	if cfg.Name != "Test Server" {
		t.Fatalf("expected decoded name Test Server, got %q", cfg.Name)
	}

	if cfg.ID == "" {
		t.Fatal("expected configuration ID to be generated")
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
		t.Fatalf("expected first configuration to be VLESS, got %q", configs[0].Type)
	}

	if configs[1].Type != config.TypeTrojan {
		t.Fatalf("expected second configuration to be Trojan, got %q", configs[1].Type)
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
		t.Fatalf(
			"expected 2 configurations from base64 input, got %d",
			len(configs),
		)
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
			"expected duplicate configurations to collapse to 1, got %d",
			len(configs),
		)
	}
}

func TestParseRejectsUnsupportedScheme(t *testing.T) {
	p := New()

	_, err := p.Parse([]byte(
		"ftp://example.com:443",
	))

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
		t.Fatalf("Parse() should keep valid entries: %v", err)
	}

	if len(configs) != 2 {
		t.Fatalf(
			"expected 2 valid configurations, got %d",
			len(configs),
		)
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
		t.Fatal("expected parsing to fail when no valid configurations exist")
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
