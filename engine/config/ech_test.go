package config

import (
	"strings"
	"testing"
)

// ech_test.go — v0.11.0 ECH model tests: normalization (raw base64 →
// PEM canonicalization), validation (TLS requirement, REALITY
// conflict, PEM shape, config XOR config_path), failure-class
// vocabulary/classification and the freshness evidence tuple.

func testECHPEM(body string) string {
	return ECHPEMBegin + "\n" + body + "\n" + ECHPEMEnd + "\n"
}

func TestNormalizeECHRawBase64WrappedIntoPEM(t *testing.T) {
	// The DNS HTTPS record `ech=` payload shape: raw base64 of an
	// ECHConfigList. Normalize must wrap it into the PEM envelope the
	// pinned sing-box v1.14.0 requires.
	raw := "AP4ADQABAAgADwAgACAAIAAgACAA" // 28 base64 chars, decodes

	cfg := Config{Type: TypeVLESS, Security: "tls", ECHEnabled: true, ECHConfig: raw}
	cfg.Normalize()

	if !strings.HasPrefix(cfg.ECHConfig, ECHPEMBegin) {
		t.Fatalf("raw base64 ECH config was not canonicalized into PEM: %q", cfg.ECHConfig)
	}

	if !strings.Contains(cfg.ECHConfig, raw) {
		t.Fatalf("PEM body must preserve the base64 payload verbatim: %q", cfg.ECHConfig)
	}
}

func TestNormalizeECHPEMPassesThrough(t *testing.T) {
	pem := testECHPEM("Z2FyYmFnZQ==")

	cfg := Config{Type: TypeVLESS, Security: "tls", ECHEnabled: true, ECHConfig: pem}
	cfg.Normalize()

	// Pass-through is CONTENT-exact: leading/trailing whitespace is
	// trimmed (the canonical form), the envelope and body untouched.
	if got, want := cfg.ECHConfig, strings.TrimSpace(pem); got != want {
		t.Fatalf("already-PEM ECH config must pass through unchanged: %q", cfg.ECHConfig)
	}
}

func TestNormalizeECHGarbageUnchanged(t *testing.T) {
	// Neither PEM nor base64 — normalization must NOT mangle it;
	// Validate rejects it with the precise reason.
	cfg := Config{Type: TypeVLESS, Security: "tls", ECHEnabled: true, ECHConfig: "!! not pem !!"}
	cfg.Normalize()

	if cfg.ECHConfig != "!! not pem !!" {
		t.Fatalf("non-PEM non-base64 value must be left for Validate to reject: %q", cfg.ECHConfig)
	}
}

func TestValidateECHAcceptsValidShapes(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{
			name: "explicit PEM config on TLS protocol",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "tls", ECHEnabled: true, ECHConfig: testECHPEM("Z2FyYmFnZQ=="),
			},
		},
		{
			name: "config_path instead of inline config",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "tls", ECHEnabled: true, ECHConfigPath: `C:\ech\config.pem`,
			},
		},
		{
			name: "query_server_name DNS discovery mode",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "tls", ECHEnabled: true, ECHQueryServerName: "dns.example.com",
			},
		},
		{
			name: "enabled bare (upstream default DNS discovery)",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "tls", ECHEnabled: true,
			},
		},
		{
			name: "hysteria2 (TLS-mandatory) with config",
			cfg: Config{
				Type: TypeHysteria2, Address: "a.example", Port: 443, Password: "p",
				ECHEnabled: true, ECHConfig: testECHPEM("Z2FyYmFnZQ=="),
			},
		},
		{
			name: "disabled with stray config field is ignored",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "tls", ECHConfig: "not even pem",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want accepted", err)
			}
		})
	}
}

func TestValidateECHRejectsInvalidShapes(t *testing.T) {
	cases := []struct {
		name       string
		cfg        Config
		wantSubstr string
	}{
		{
			name: "plaintext protocol",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				ECHEnabled: true,
			},
			wantSubstr: "ECH requires TLS security",
		},
		{
			name: "REALITY conflict (verified binary behavior)",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "reality", PublicKey: "pbk", ECHEnabled: true,
			},
			wantSubstr: "REALITY conflicts with ECH",
		},
		{
			name: "WireGuard has no TLS layer",
			cfg: Config{
				Type: TypeWireGuard, Address: "a.example", Port: 51820,
				PrivateKey: "k", PublicKey: "pk", ECHEnabled: true,
			},
			wantSubstr: "no TLS layer",
		},
		{
			name: "raw base64 rejected after explicit non-canonical input",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "tls", ECHEnabled: true, ECHConfig: "not-pem-or-base64!!!",
			},
			wantSubstr: "must be a PEM",
		},
		{
			name: "wrong PEM header spelling",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "tls", ECHEnabled: true,
				ECHConfig: "-----BEGIN ECH CONFIG-----\nZ2FyYmFnZQ==\n-----END ECH CONFIG-----\n",
			},
			wantSubstr: "must be a PEM",
		},
		{
			name: "PEM with non-base64 body",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "tls", ECHEnabled: true,
				ECHConfig: ECHPEMBegin + "\n!!!not-base64!!!\n" + ECHPEMEnd + "\n",
			},
			wantSubstr: "must be a PEM",
		},
		{
			name: "config and config_path together are ambiguous",
			cfg: Config{
				Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
				Security: "tls", ECHEnabled: true,
				ECHConfig: testECHPEM("Z2FyYmFnZQ=="), ECHConfigPath: "/etc/ech.pem",
			},
			wantSubstr: "mutually exclusive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want rejection")
			}

			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Validate() = %v, want containing %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestValidateECHTLSCertificateStillEnforced(t *testing.T) {
	// ECH validation must not shadow the protocol's own requirements.
	cfg := Config{Type: TypeTrojan, Address: "a.example", Port: 443, Security: "tls", ECHEnabled: true}
	err := cfg.Validate()

	if err == nil || !strings.Contains(err.Error(), "Trojan password") {
		t.Fatalf("Validate() = %v, want the Trojan password requirement to fire", err)
	}
}

func TestValidECHPEMLineWrappedBody(t *testing.T) {
	// PEM bodies are conventionally line-wrapped at 64 columns; a
	// multi-line body where every line is valid base64 must pass.
	pem := ECHPEMBegin + "\n" +
		"AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH\n" +
		"IIIIJJJJKKKKLLLLMMMMNNNNOOOOPPPP\n" +
		ECHPEMEnd + "\n"

	cfg := Config{Type: TypeVLESS, Address: "a.example", Port: 443, UUID: "u",
		Security: "tls", ECHEnabled: true, ECHConfig: pem}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want line-wrapped PEM accepted", err)
	}
}
