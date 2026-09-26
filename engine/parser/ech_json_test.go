package parser

import (
	"strings"
	"testing"
)

// ech_json_test.go — v0.11.0: ECH fields enter through the canonical
// structured/JSON representation. The URI share formats carry NO
// standardized ECH parameter (the ecosystem has not agreed on one),
// so nothing is invented for URIs — the parser must neither invent
// ech= query parameters nor silently drop ECH from structured input.

const echPEMFixture = "-----BEGIN ECH CONFIGS-----\n" +
	"AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH\n" +
	"-----END ECH CONFIGS-----"

func TestParseJSONCapturesECHFields(t *testing.T) {
	input := `{
		"type": "trojan",
		"name": "ech-node",
		"address": "ech.example.org",
		"port": 443,
		"password": "pw",
		"security": "tls",
		"ech_enabled": true,
		"ech_config": "` + strings.ReplaceAll(echPEMFixture, "\n", "\\n") + `",
		"ech_query_server_name": "dns.example.org"
	}`

	parser := New()

	configs, err := parser.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("configs = %d, want 1", len(configs))
	}

	cfg := configs[0]

	if !cfg.ECHEnabled {
		t.Fatal("ech_enabled not captured from structured JSON")
	}

	if !strings.HasPrefix(cfg.ECHConfig, "-----BEGIN ECH CONFIGS-----") {
		t.Fatalf("ech_config not captured: %q", cfg.ECHConfig)
	}

	if cfg.ECHQueryServerName != "dns.example.org" {
		t.Fatalf("ech_query_server_name = %q", cfg.ECHQueryServerName)
	}

	if cfg.ECHConfigPath != "" {
		t.Fatalf("ech_config_path must stay empty when absent: %q", cfg.ECHConfigPath)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the parsed ECH config must validate: %v", err)
	}
}

func TestParseJSONCapturesECHConfigPath(t *testing.T) {
	input := `{
		"type": "vless",
		"address": "ech2.example.org",
		"port": 443,
		"uuid": "11111111-1111-1111-1111-111111111111",
		"security": "tls",
		"ech_enabled": true,
		"ech_config_path": "C:\\ech\\list.pem"
	}`

	parser := New()

	configs, err := parser.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	if len(configs) != 1 || configs[0].ECHConfigPath != `C:\ech\list.pem` {
		t.Fatalf("ech_config_path not captured: %+v", configs)
	}
}

func TestParseJSONRawBase64ECHNormalized(t *testing.T) {
	// A raw base64 ECHConfigList (the DNS HTTPS record `ech=` payload
	// shape) is canonicalized into the PEM envelope during
	// normalization, then validates.
	input := `{
		"type": "trojan",
		"address": "ech3.example.org",
		"port": 443,
		"password": "pw",
		"security": "tls",
		"ech_enabled": true,
		"ech_config": "AAAABBBBCCCCDDDDEEEEFFFF"
	}`

	parser := New()

	configs, err := parser.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	cfg := configs[0]

	if !strings.HasPrefix(cfg.ECHConfig, "-----BEGIN ECH CONFIGS-----") {
		t.Fatalf("raw base64 ECH payload must be normalized into PEM: %q", cfg.ECHConfig)
	}

	if !strings.Contains(cfg.ECHConfig, "AAAABBBBCCCCDDDDEEEEFFFF") {
		t.Fatalf("PEM body must preserve the payload: %q", cfg.ECHConfig)
	}
}

func TestParseJSONECHDisabledByDefault(t *testing.T) {
	input := `{
		"type": "trojan",
		"address": "plain.example.org",
		"port": 443,
		"password": "pw",
		"security": "tls"
	}`

	parser := New()

	configs, err := parser.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	if configs[0].ECHEnabled {
		t.Fatal("ECH must stay disabled without ech_enabled")
	}
}

// TestParseURIDoesNotInventECHParameters pins the honesty rule: URI
// share formats carry no standardized ECH parameter, and the parser
// must not invent one (a fake ech= parameter would silently change
// TLS behavior or be dropped by every core).
func TestParseURIDoesNotInventECHParameters(t *testing.T) {
	// Even if a source smuggles ech= into a URI query parameter, the
	// parser does not promote it: no field in the ecosystem carries
	// that meaning.
	input := "trojan://pw@ech.example.org:443?security=tls&ech=whatever#" +
		"ech-node"

	parser := New()

	configs, err := parser.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	cfg := configs[0]

	if cfg.ECHEnabled || cfg.ECHConfig != "" ||
		cfg.ECHConfigPath != "" || cfg.ECHQueryServerName != "" {
		t.Fatalf("URI parameters must not invent ECH state: %+v", cfg)
	}
}
