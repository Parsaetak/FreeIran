package parser_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/parser"
)

func mustParseOne(t *testing.T, data string) config.Config {
	t.Helper()

	p := parser.New()

	configs, err := p.Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("config count = %d, want 1", len(configs))
	}

	return configs[0]
}

// TestVLESSProtocolDetails verifies the parser captures flow,
// encryption, alpn and spider fields consumed by core backends.
func TestVLESSProtocolDetails(t *testing.T) {
	cfg := mustParseOne(t,
		"vless://11111111-1111-1111-1111-111111111111@detail.example.org:443"+
			"?type=tcp&security=reality&pbk=49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY"+
			"&sid=0123456789abcdef&fp=chrome&flow=xtls-rprx-vision&spx=%2F"+
			"&sni=www.example.org&alpn=h2,http/1.1#details")

	if cfg.Flow != "xtls-rprx-vision" {
		t.Fatalf("flow = %s", cfg.Flow)
	}

	if cfg.Encryption != "" {
		t.Fatalf("encryption = %s, want empty (not published)", cfg.Encryption)
	}

	if cfg.PublicKey != "49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY" {
		t.Fatalf("pbk = %s", cfg.PublicKey)
	}

	if cfg.ShortID != "0123456789abcdef" {
		t.Fatalf("sid = %s", cfg.ShortID)
	}

	if cfg.FingerprintProfile != "chrome" {
		t.Fatalf("fp = %s", cfg.FingerprintProfile)
	}

	if cfg.SpiderX != "/" {
		t.Fatalf("spx = %s", cfg.SpiderX)
	}

	if cfg.ServerName != "www.example.org" {
		t.Fatalf("sni = %s", cfg.ServerName)
	}

	if len(cfg.ALPN) != 2 || cfg.ALPN[0] != "h2" || cfg.ALPN[1] != "http/1.1" {
		t.Fatalf("alpn = %v", cfg.ALPN)
	}
}

// TestVLESSEncryptionParam verifies the encryption parameter.
func TestVLESSEncryptionParam(t *testing.T) {
	cfg := mustParseOne(t,
		"vless://11111111-1111-1111-1111-111111111111@enc.example.org:443"+
			"?type=ws&security=tls&encryption=none#enc")

	if cfg.Encryption != "none" {
		t.Fatalf("encryption = %s", cfg.Encryption)
	}
}

// TestVMessProtocolDetails verifies alterId and header-type capture.
func TestVMessProtocolDetails(t *testing.T) {
	payload := map[string]any{
		"ps":   "vmess-details",
		"add":  "vmess.example.org",
		"port": 443,
		"id":   "33333333-3333-3333-3333-333333333333",
		"aid":  "0",
		"net":  "tcp",
		"type": "http",
		"host": "vmess.example.org",
		"path": "/vm",
		"tls":  "tls",
		"sni":  "vmess.example.org",
		"scy":  "auto",
	}

	raw, _ := json.Marshal(payload)

	link := "vmess://" + base64.StdEncoding.EncodeToString(raw)

	cfg := mustParseOne(t, link)

	if cfg.HeaderType != "http" {
		t.Fatalf("header type = %s", cfg.HeaderType)
	}

	if cfg.Encryption != "auto" {
		t.Fatalf("scy = %s", cfg.Encryption)
	}

	if cfg.AlterID != 0 {
		t.Fatalf("aid = %d", cfg.AlterID)
	}
}

// TestV2RayJSONOutbounds verifies complete V2Ray client JSON shares
// are parsed into normalized configurations.
func TestV2RayJSONOutbounds(t *testing.T) {
	document := `{
		"log": {"loglevel": "warning"},
		"inbounds": [
			{"listen": "127.0.0.1", "port": 10808, "protocol": "socks"}
		],
		"outbounds": [
			{
				"tag": "proxy",
				"protocol": "vless",
				"settings": {
					"vnext": [{
						"address": "json-vless.example.org",
						"port": 443,
						"users": [{
							"id": "44444444-4444-4444-4444-444444444444",
							"encryption": "none",
							"flow": "xtls-rprx-vision"
						}]
					}]
				},
				"streamSettings": {
					"network": "tcp",
					"security": "reality",
					"realitySettings": {
						"serverName": "www.example.org",
						"fingerprint": "chrome",
						"publicKey": "49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY",
						"shortId": "0123456789abcdef"
					}
				}
			},
			{"tag": "direct", "protocol": "freedom"},
			{"tag": "block", "protocol": "blackhole"}
		]
	}`

	p := parser.New()

	configs, err := p.Parse([]byte(document))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("config count = %d, want 1 (freedom/blackhole skipped)", len(configs))
	}

	cfg := configs[0]

	if cfg.Type != config.TypeVLESS {
		t.Fatalf("type = %s", cfg.Type)
	}

	if cfg.Address != "json-vless.example.org" || cfg.Port != 443 {
		t.Fatalf("endpoint = %s:%d", cfg.Address, cfg.Port)
	}

	if cfg.UUID != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("uuid = %s", cfg.UUID)
	}

	if cfg.Flow != "xtls-rprx-vision" {
		t.Fatalf("flow = %s", cfg.Flow)
	}

	if cfg.Security != "reality" {
		t.Fatalf("security = %s", cfg.Security)
	}

	if cfg.PublicKey != "49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY" {
		t.Fatalf("pbk = %s", cfg.PublicKey)
	}

	if cfg.ServerName != "www.example.org" {
		t.Fatalf("sni = %s", cfg.ServerName)
	}
}

// TestV2RayJSONVMessTrojanSS verifies the remaining protocol
// conversions from V4 outbounds.
func TestV2RayJSONVMessTrojanSS(t *testing.T) {
	document := `{
		"outbounds": [
			{
				"tag": "vm",
				"protocol": "vmess",
				"settings": {"vnext": [{
					"address": "json-vmess.example.org",
					"port": 443,
					"users": [{"id": "55555555-5555-5555-5555-555555555555", "alterId": 0}]
				}]},
				"streamSettings": {"network": "ws", "security": "tls",
					"wsSettings": {"path": "/ws", "headers": {"Host": "json-vmess.example.org"}}}
			},
			{
				"tag": "tr",
				"protocol": "trojan",
				"settings": {"servers": [{
					"address": "json-trojan.example.org",
					"port": 443,
					"password": "synthetic-trojan-password"
				}]}
			},
			{
				"tag": "ss",
				"protocol": "shadowsocks",
				"settings": {"servers": [{
					"address": "json-ss.example.org",
					"port": 8388,
					"method": "AES-256-GCM",
					"password": "synthetic-ss-password"
				}]}
			}
		]
	}`

	p := parser.New()

	configs, err := p.Parse([]byte(document))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	if len(configs) != 3 {
		t.Fatalf("config count = %d, want 3", len(configs))
	}

	vmess := configs[0]

	if vmess.Type != config.TypeVMess || vmess.Network != "ws" ||
		vmess.Path != "/ws" || vmess.Host != "json-vmess.example.org" {
		t.Fatalf("vmess = %+v", vmess)
	}

	trojan := configs[1]

	if trojan.Type != config.TypeTrojan || trojan.Password != "synthetic-trojan-password" {
		t.Fatalf("trojan = %+v", trojan)
	}

	ss := configs[2]

	if ss.Type != config.TypeShadowsocks || ss.Method != "aes-256-gcm" {
		t.Fatalf("shadowsocks = %+v", ss)
	}
}

// TestV2RayJSONInvalidPorts verifies invalid ports are rejected
// through validation.
func TestV2RayJSONInvalidPorts(t *testing.T) {
	document := `{
		"outbounds": [
			{"protocol": "vless", "settings": {"vnext": [{
				"address": "bad.example.org",
				"port": 99999,
				"users": [{"id": "66666666-6666-6666-6666-666666666666"}]
			}]}}
		]
	}`

	p := parser.New()

	configs, err := p.Parse([]byte(document))
	if err == nil && len(configs) > 0 {
		t.Fatal("invalid port should be rejected by validation")
	}
}

// TestParserRejectsOversizedInput verifies the input boundary.
func TestParserRejectsOversizedInput(t *testing.T) {
	p := parser.New()

	huge := []byte(strings.Repeat("vless://x\n", (parser.MaxInputSize/(10))+1))

	_, err := p.Parse(huge)
	if err == nil {
		t.Fatal("oversized input should be rejected")
	}
}

// TestParserNeverPanicsOnHostileInput fuzzes the parser with hostile
// inputs (§24: parsers must never panic on malformed input).
func TestParserNeverPanicsOnHostileInput(t *testing.T) {
	hostile := []string{
		"vless://",
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"port":"abc"}`)),
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"port":[1,2]}`)),
		"vmess://!!!!not-base64!!!!",
		"trojan://@/",
		"ss://",
		"tuic://:1",
		`{"outbounds": "not-an-array"}`,
		`{"outbounds": [null, 42, "x", {}]}`,
		`{"outbounds": [{"protocol": 42}]}`,
		strings.Repeat("{", 64),
		strings.Repeat("[", 64),
		"\x00\x01\x02\x03",
		"vless://" + strings.Repeat("a", 1<<16) + "@x:1",
	}

	p := parser.New()

	for _, input := range hostile {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("parser panicked on %q: %v",
						truncate(input, 64), r)
				}
			}()

			_, _ = p.Parse([]byte(input)) //nolint:errcheck // fuzz
		}()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "..."
}
