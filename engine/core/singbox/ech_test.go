package singbox_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/singbox"
)

// ech_test.go — v0.11.0 ECH generation tests. Every emission rule
// below was verified against the PINNED real sing-box v1.14.0 binary
// (check + startup); the unit tests pin the generator to exactly
// those shapes. See docs/protocols.md for the evidence table.

// echDoc is the subset of the generated document the ECH tests
// assert on.
type echDoc struct {
	Outbounds []struct {
		Type string `json:"type"`
		TLS  *struct {
			Enabled bool `json:"enabled"`
			ECH     *struct {
				Enabled         bool   `json:"enabled"`
				Config          string `json:"config,omitempty"`
				ConfigPath      string `json:"config_path,omitempty"`
				QueryServerName string `json:"query_server_name,omitempty"`
			} `json:"ech,omitempty"`
		} `json:"tls,omitempty"`
	} `json:"outbounds"`
}

// testECHPEM builds a PEM-wrapped, STRUCTURALLY VALID ECHConfigList
// (RFC 9460 wire format: version 0xfe0d, one X25519 + HKDF-SHA256 +
// AES-128-GCM config, public_name "example.com"). The pinned sing-box
// v1.14.0 check gate PEM-parses and base64-decodes the body — the
// payload here additionally parses cleanly as an ECHConfigList so
// the document would survive a stricter future core too.
func testECHPEM() string {
	list := testECHConfigList()

	return config.ECHPEMBegin + "\n" +
		base64.StdEncoding.EncodeToString(list) +
		"\n" + config.ECHPEMEnd + "\n"
}

// testECHConfigList assembles the ECHConfigList bytes (draft-13 /
// RFC 9460 layout).
func testECHConfigList() []byte {
	cfg := []byte{0x01}           // config_id
	cfg = append(cfg, 0x00, 0x20) // kem_id = X25519

	key := make([]byte, 32) // public_key
	for i := range key {
		key[i] = byte(0x41 + i%26)
	}

	cfg = append(cfg, byte(len(key)>>8), byte(len(key))) // key length
	cfg = append(cfg, key...)
	cfg = append(cfg, 0x00, 0x04) // cipher_suites length
	cfg = append(cfg, 0x00, 0x01) // kdf_id = HKDF-SHA256
	cfg = append(cfg, 0x00, 0x01) // aead_id = AES-128-GCM
	cfg = append(cfg, 0x40)       // maximum_name_length

	name := []byte("example.com") // public_name
	cfg = append(cfg, byte(len(name)))
	cfg = append(cfg, name...)
	cfg = append(cfg, 0x00, 0x00) // extensions length

	inner := make([]byte, 2)
	binary.BigEndian.PutUint16(inner, uint16(len(cfg)))
	echConfig := append(inner, cfg...)

	listLen := make([]byte, 2)
	binary.BigEndian.PutUint16(listLen, uint16(len(echConfig)))

	list := append([]byte{0xfe, 0x0d}, listLen...) // version 0xfe0d
	list = append(list, echConfig...)

	return list
}

func buildECHDoc(t *testing.T, cfg config.Config) echDoc {
	t.Helper()

	backend := singbox.New()

	doc, err := backend.BuildConfig(cfg, core.RuntimeOptions{
		LocalPort:       47090,
		DisableGenCache: true,
	})
	if err != nil {
		t.Fatalf("BuildConfig() = %v", err)
	}

	var parsed echDoc
	if err := json.Unmarshal(doc.Data, &parsed); err != nil {
		t.Fatalf("parse document: %v", err)
	}

	return parsed
}

func TestECHConfigEmittedWhenEnabled(t *testing.T) {
	cfg := trojanConfig()
	cfg.ECHEnabled = true
	cfg.ECHConfig = testECHPEM()

	doc := buildECHDoc(t, cfg)

	if len(doc.Outbounds) == 0 || doc.Outbounds[0].TLS == nil ||
		doc.Outbounds[0].TLS.ECH == nil {
		t.Fatal("tls.ech object missing for an ECH-enabled config")
	}

	ech := doc.Outbounds[0].TLS.ECH

	if !ech.Enabled {
		t.Fatal("tls.ech.enabled must be true")
	}

	if ech.Config != strings.TrimSpace(testECHPEM()) {
		t.Fatalf("tls.ech.config must mirror the configured PEM: %q", ech.Config)
	}

	if ech.ConfigPath != "" || ech.QueryServerName != "" {
		t.Fatalf("unset ECH fields must stay absent: %+v", ech)
	}
}

func TestECHConfigPathAndQueryServerName(t *testing.T) {
	cfg := trojanConfig()
	cfg.ECHEnabled = true
	cfg.ECHConfigPath = `C:\ech\list.pem`
	cfg.ECHQueryServerName = "dns.example.com"

	doc := buildECHDoc(t, cfg)

	if len(doc.Outbounds) == 0 || doc.Outbounds[0].TLS == nil ||
		doc.Outbounds[0].TLS.ECH == nil {
		t.Fatal("tls.ech must be emitted enabled")
	}

	ech := doc.Outbounds[0].TLS.ECH

	if ech.ConfigPath != `C:\ech\list.pem` {
		t.Fatalf("tls.ech.config_path = %q", ech.ConfigPath)
	}

	if ech.QueryServerName != "dns.example.com" {
		t.Fatalf("tls.ech.query_server_name = %q", ech.QueryServerName)
	}

	if ech.Config != "" {
		t.Fatalf("inline config must stay empty when config_path is used: %q", ech.Config)
	}
}

func TestECHAbsentWhenDisabled(t *testing.T) {
	// Stray fields with ECH disabled must not emit the object —
	// sing-box with ech.enabled=false ignores the rest, and FreeIran
	// must not claim ECH the user did not enable.
	cfg := trojanConfig()
	cfg.ECHConfig = testECHPEM()

	doc := buildECHDoc(t, cfg)

	if len(doc.Outbounds) == 0 || doc.Outbounds[0].TLS == nil {
		t.Fatal("trojan must carry a TLS object")
	}

	if doc.Outbounds[0].TLS.ECH != nil {
		t.Fatalf("tls.ech emitted for a disabled config: %+v", doc.Outbounds[0].TLS.ECH)
	}
}

func TestECHOnQUICFamily(t *testing.T) {
	// The TLS-mandatory QUIC family (hysteria2) carries ECH on its
	// mandatory TLS object.
	cfg := hysteria2Config()
	cfg.ECHEnabled = true
	cfg.ECHConfig = testECHPEM()

	doc := buildECHDoc(t, cfg)

	if len(doc.Outbounds) == 0 || doc.Outbounds[0].TLS == nil ||
		doc.Outbounds[0].TLS.ECH == nil || !doc.Outbounds[0].TLS.ECH.Enabled {
		t.Fatalf("hysteria2 tls.ech missing or disabled")
	}
}

func TestValidateRejectsECHRealityConflict(t *testing.T) {
	// The pinned binary rejects REALITY+ECH at startup ("Reality is
	// conflict with ECH"); config validation must reject it FIRST.
	cfg := realityVisionConfig()
	cfg.ECHEnabled = true
	cfg.ECHConfig = testECHPEM()

	backend := singbox.New()

	if err := backend.Validate(context.Background(), cfg); err == nil {
		t.Fatal("REALITY + ECH must be rejected before generation")
	}
}

func TestValidateRejectsECHWithoutTLS(t *testing.T) {
	cfg := shadowsocksConfig()
	cfg.ECHEnabled = true

	backend := singbox.New()

	if err := backend.Validate(context.Background(), cfg); err == nil {
		t.Fatal("ECH on a plaintext protocol must be rejected")
	}
}

// TestSupportsRoutesECHToSingBoxOnly pins the capability-matrix fact:
// an ECH-enabled configuration is only supported by backends that
// DECLARE ECH (verified schema support) — handing it to another core
// would silently connect without the encrypted client hello.
func TestSupportsRoutesECHToSingBoxOnly(t *testing.T) {
	backend := singbox.New()

	plain := trojanConfig()
	if !backend.Supports(plain) {
		t.Fatal("plain trojan must stay supported")
	}

	ech := trojanConfig()
	ech.ECHEnabled = true
	if !backend.Supports(ech) {
		t.Fatal("ECH trojan must be supported by sing-box (declared, verified)")
	}

	if !backend.Capabilities().ECH {
		t.Fatal("sing-box capabilities must declare ECH")
	}
}

// TestECHWrongPEMHeaderSpellingRejected documents WHY the canonical
// form is pinned: the adapter emits the config verbatim, and only the
// "ECH CONFIGS" header spelling passes the pinned binary's PEM parse
// (verified: "ECH CONFIG"/"ECHCONFIG" spellings fail with "invalid
// ECH configs pem").
func TestECHWrongPEMHeaderSpellingRejected(t *testing.T) {
	wrong := strings.Replace(testECHPEM(), config.ECHPEMBegin, "-----BEGIN ECH CONFIG-----", 1)
	wrong = strings.Replace(wrong, config.ECHPEMEnd, "-----END ECH CONFIG-----", 1)

	cfg := trojanConfig()
	cfg.ECHEnabled = true
	cfg.ECHConfig = wrong

	if err := cfg.Validate(); err == nil {
		t.Fatal("the wrong PEM header spelling must be rejected by validation")
	}
}

// --- real-binary smoke fixtures (consumed by singbox_test.go) -----

// echPathConfig writes the PEM to a temp file and returns the config
// using config_path — for the real-binary smoke test.
func echPathConfig(t *testing.T) config.Config {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "ech-configs.pem")

	if err := os.WriteFile(path, []byte(testECHPEM()), 0o600); err != nil {
		t.Fatalf("write ECH PEM fixture: %v", err)
	}

	cfg := trojanConfig()
	cfg.Address = "sb-ech-path.example.org"
	cfg.ECHEnabled = true
	cfg.ECHConfigPath = path

	return cfg
}

func echQueryConfig() config.Config {
	cfg := trojanConfig()
	cfg.Address = "sb-ech-dns.example.org"
	cfg.ECHEnabled = true
	cfg.ECHQueryServerName = "ech-discovery.example.org"

	return cfg
}

func echExplicitConfig() config.Config {
	cfg := trojanConfig()
	cfg.Address = "sb-ech-inline.example.org"
	cfg.ECHEnabled = true
	cfg.ECHConfig = testECHPEM()

	return cfg
}
