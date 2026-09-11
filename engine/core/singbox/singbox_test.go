package singbox_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/singbox"
)

// TestContract runs the shared backend contract suite against the
// sing-box adapter with the fake-core process stand-in.
func TestContract(t *testing.T) {
	contract.Run(t, contract.Suite{
		Backend:    singbox.New(),
		FakeBinary: contract.BuildFakeCore(t),
		Supported: []contract.Case{
			{Name: "vless-tcp-tls", Config: vlessTLSConfig()},
			{Name: "vless-ws", Config: vlessWSConfig()},
			{Name: "vmess-grpc", Config: vmessGRPCConfig()},
			{Name: "trojan-tcp", Config: trojanConfig()},
			{Name: "shadowsocks", Config: shadowsocksConfig()},
			{Name: "reality-vision", Config: realityVisionConfig()},
			{Name: "vmess-quic", Config: quicConfig()},
			{Name: "vmess-h2", Config: h2Config()},
			{Name: "socks", Config: socksConfig()},
			{Name: "http", Config: httpConfig()},
		},
		Unsupported: []contract.Case{
			{Name: "hysteria2-unsupported-in-v0.4", Config: hysteria2Config()},
			{Name: "tuic-unsupported-in-v0.4", Config: tuicConfig()},
		},
	})
}

// sbDoc mirrors the sing-box document shape for assertions.
type sbDoc struct {
	Log struct {
		Level string `json:"level"`
	} `json:"log"`
	Inbounds []struct {
		Type       string  `json:"type"`
		Listen     string  `json:"listen"`
		ListenPort float64 `json:"listen_port"`
	} `json:"inbounds"`
	Outbounds []struct {
		Type       string          `json:"type"`
		Tag        string          `json:"tag"`
		Server     string          `json:"server"`
		ServerPort float64         `json:"server_port"`
		UUID       string          `json:"uuid"`
		Password   string          `json:"password"`
		Method     string          `json:"method"`
		Flow       string          `json:"flow"`
		TLS        json.RawMessage `json:"tls"`
		Transport  json.RawMessage `json:"transport"`
	} `json:"outbounds"`
	Route *struct {
		Rules []map[string]any `json:"rules"`
		Final string           `json:"final"`
	} `json:"route"`
}

func buildFor(t *testing.T, cfg config.Config) sbDoc {
	t.Helper()

	backend := singbox.New()

	doc, err := backend.BuildConfig(cfg, core.RuntimeOptions{
		LocalPort:       47090,
		DisableGenCache: true,
	})
	if err != nil {
		t.Fatalf("BuildConfig() = %v", err)
	}

	var parsed sbDoc

	if err := json.Unmarshal(doc.Data, &parsed); err != nil {
		t.Fatalf("parse document: %v", err)
	}

	return parsed
}

// commonStructure asserts the sing-box skeleton: mixed inbound,
// proxy/direct/block outbounds, private-range direct route.
func commonStructure(t *testing.T, parsed sbDoc) {
	t.Helper()

	if parsed.Log.Level != "warn" {
		t.Fatalf("log level = %s, want warn", parsed.Log.Level)
	}

	if len(parsed.Inbounds) != 1 {
		t.Fatalf("inbound count = %d, want 1 (mixed)", len(parsed.Inbounds))
	}

	inbound := parsed.Inbounds[0]

	if inbound.Type != "mixed" || inbound.Listen != "127.0.0.1" {
		t.Fatalf("inbound = %+v, want local mixed", inbound)
	}

	if inbound.ListenPort != 47090 {
		t.Fatalf("listen_port = %v, want 47090", inbound.ListenPort)
	}

	if len(parsed.Outbounds) < 3 {
		t.Fatalf("outbound count = %d, want >= 3", len(parsed.Outbounds))
	}

	if parsed.Outbounds[0].Tag != "proxy" {
		t.Fatalf("first outbound tag = %s, want proxy", parsed.Outbounds[0].Tag)
	}

	if parsed.Route == nil || parsed.Route.Final != "proxy" {
		t.Fatalf("route = %+v, want final=proxy", parsed.Route)
	}

	rule := parsed.Route.Rules[0]

	if rule["outbound"] != "direct" || rule["ip_is_private"] != true {
		t.Fatalf("first rule = %v, want private→direct", rule)
	}
}

// TestVLESSConfig verifies the VLESS → sing-box conversion.
func TestVLESSConfig(t *testing.T) {
	parsed := buildFor(t, vlessTLSConfig())

	commonStructure(t, parsed)

	proxy := parsed.Outbounds[0]

	if proxy.Type != "vless" {
		t.Fatalf("type = %s, want vless", proxy.Type)
	}

	if proxy.Server != "sb-vless.example.org" || proxy.ServerPort != 443 {
		t.Fatalf("server = %+v", proxy)
	}

	if proxy.UUID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("uuid = %s", proxy.UUID)
	}

	var tls struct {
		Enabled    bool   `json:"enabled"`
		ServerName string `json:"server_name"`
	}

	if err := json.Unmarshal(proxy.TLS, &tls); err != nil {
		t.Fatalf("tls: %v", err)
	}

	if !tls.Enabled || tls.ServerName != "sb-vless.example.org" {
		t.Fatalf("tls = %+v", tls)
	}
}

// TestTrojanConfig verifies the Trojan → sing-box conversion with
// mandatory TLS.
func TestTrojanConfig(t *testing.T) {
	cfg := trojanConfig()
	cfg.Security = "" // trojan implies TLS

	parsed := buildFor(t, cfg)

	commonStructure(t, parsed)

	proxy := parsed.Outbounds[0]

	if proxy.Type != "trojan" {
		t.Fatalf("type = %s, want trojan", proxy.Type)
	}

	if proxy.Password != "synthetic-trojan-password" {
		t.Fatalf("password = %s", proxy.Password)
	}

	var tls struct {
		Enabled bool `json:"enabled"`
	}

	if err := json.Unmarshal(proxy.TLS, &tls); err != nil {
		t.Fatalf("tls: %v", err)
	}

	if !tls.Enabled {
		t.Fatal("trojan without tls should default to enabled TLS on sing-box")
	}
}

// TestREALITYConfig verifies REALITY emission: tls.reality +
// utls fingerprint + vision flow.
func TestREALITYConfig(t *testing.T) {
	parsed := buildFor(t, realityVisionConfig())

	commonStructure(t, parsed)

	proxy := parsed.Outbounds[0]

	if proxy.Flow != "xtls-rprx-vision" {
		t.Fatalf("flow = %s, want vision", proxy.Flow)
	}

	var tls struct {
		Enabled bool `json:"enabled"`
		UTLS    *struct {
			Enabled     bool   `json:"enabled"`
			Fingerprint string `json:"fingerprint"`
		} `json:"utls"`
		Reality *struct {
			Enabled   bool   `json:"enabled"`
			PublicKey string `json:"public_key"`
			ShortID   string `json:"short_id"`
		} `json:"reality"`
	}

	if err := json.Unmarshal(proxy.TLS, &tls); err != nil {
		t.Fatalf("tls: %v", err)
	}

	if tls.UTLS == nil || !tls.UTLS.Enabled || tls.UTLS.Fingerprint != "chrome" {
		t.Fatalf("utls = %+v", tls.UTLS)
	}

	if tls.Reality == nil || !tls.Reality.Enabled {
		t.Fatal("reality object missing")
	}

	if tls.Reality.PublicKey != "49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY" {
		t.Fatalf("public_key = %s", tls.Reality.PublicKey)
	}
}

// TestTransportEmission verifies ws/grpc/h2/quic transport objects.
func TestTransportEmission(t *testing.T) {
	ws := buildFor(t, vlessWSConfig())

	if string(ws.Outbounds[0].Transport) == "" {
		t.Fatal("ws config has no transport object")
	}

	if !jsonFieldEquals(ws.Outbounds[0].Transport, "type", "ws") {
		t.Fatalf("ws transport = %s", ws.Outbounds[0].Transport)
	}

	grpc := buildFor(t, vmessGRPCConfig())

	if !jsonFieldEquals(grpc.Outbounds[0].Transport, "type", "grpc") {
		t.Fatalf("grpc transport = %s", grpc.Outbounds[0].Transport)
	}

	quic := buildFor(t, quicConfig())

	if !jsonFieldEquals(quic.Outbounds[0].Transport, "type", "quic") {
		t.Fatalf("quic transport = %s", quic.Outbounds[0].Transport)
	}
}

// TestSingBoxSmokeRealBinary validates the adapter against a real
// sing-box binary (FREEIRAN_TEST_SINGBOX_BIN).
func TestSingBoxSmokeRealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode skips real-binary smoke tests")
	}

	contract.RunSmoke(t, contract.SmokeOptions{
		BinaryPath: os.Getenv("FREEIRAN_TEST_SINGBOX_BIN"),
		Backend:    singbox.New(),
		Configs: []config.Config{
			vlessTLSConfig(),
			vlessWSConfig(),
			vmessGRPCConfig(),
			trojanConfig(),
			shadowsocksConfig(),
			realityVisionConfig(),
			quicConfig(),
			h2Config(),
			socksConfig(),
			httpConfig(),
		},
		ValidateArgs: func(file string) []string {
			return []string{"check", "-c", file}
		},
		Timeout: 20 * time.Second,
	})
}

// TestValidateRejectsHysteria2 documents the v0.4 boundary: the
// adapter does not yet emit hysteria2/tuic outbounds.
func TestValidateRejectsHysteria2(t *testing.T) {
	backend := singbox.New()

	if err := backend.Validate(context.Background(), hysteria2Config()); err == nil {
		t.Fatal("hysteria2 should be rejected by the v0.4 sing-box adapter")
	}
}

// --- fixtures ---

func vlessTLSConfig() config.Config {
	return config.Config{
		Type:       config.TypeVLESS,
		Address:    "sb-vless.example.org",
		Port:       443,
		UUID:       "11111111-1111-1111-1111-111111111111",
		Network:    "tcp",
		Security:   "tls",
		ServerName: "sb-vless.example.org",
	}
}

func vlessWSConfig() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "sb-ws.example.org",
		Port:     443,
		UUID:     "22222222-2222-2222-2222-222222222222",
		Network:  "ws",
		Security: "tls",
		Path:     "/tunnel",
		Host:     "sb-ws.example.org",
	}
}

func vmessGRPCConfig() config.Config {
	return config.Config{
		Type:     config.TypeVMess,
		Address:  "sb-grpc.example.org",
		Port:     443,
		UUID:     "33333333-3333-3333-3333-333333333333",
		Network:  "grpc",
		Security: "tls",
		Service:  "svc",
	}
}

func trojanConfig() config.Config {
	return config.Config{
		Type:     config.TypeTrojan,
		Address:  "sb-trojan.example.org",
		Port:     443,
		Password: "synthetic-trojan-password",
		Network:  "tcp",
		Security: "tls",
	}
}

func shadowsocksConfig() config.Config {
	return config.Config{
		Type:     config.TypeShadowsocks,
		Address:  "sb-ss.example.org",
		Port:     8388,
		Method:   "aes-256-gcm",
		Password: "synthetic-ss-password",
	}
}

func realityVisionConfig() config.Config {
	return config.Config{
		Type:               config.TypeVLESS,
		Address:            "sb-reality.example.org",
		Port:               443,
		UUID:               "66666666-6666-6666-6666-666666666666",
		Network:            "tcp",
		Security:           "reality",
		ServerName:         "www.example.org",
		FingerprintProfile: "chrome",
		PublicKey:          "49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY",
		ShortID:            "0123456789abcdef",
		Flow:               "xtls-rprx-vision",
	}
}

func quicConfig() config.Config {
	return config.Config{
		Type:     config.TypeVMess,
		Address:  "sb-quic.example.org",
		Port:     443,
		UUID:     "44444444-4444-4444-4444-444444444444",
		Network:  "quic",
		Security: "tls",
	}
}

func h2Config() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "sb-h2.example.org",
		Port:     8443,
		UUID:     "55555555-5555-5555-5555-555555555555",
		Network:  "http",
		Security: "tls",
		Path:     "/h2",
	}
}

func socksConfig() config.Config {
	return config.Config{
		Type:     config.TypeSOCKS,
		Address:  "sb-socks.example.org",
		Port:     1080,
		Username: "synthetic-user",
		Password: "synthetic-pass",
	}
}

func httpConfig() config.Config {
	return config.Config{
		Type:     config.TypeHTTP,
		Address:  "sb-http.example.org",
		Port:     8080,
		Username: "synthetic-user",
		Password: "synthetic-pass",
	}
}

func hysteria2Config() config.Config {
	return config.Config{
		Type:     config.TypeHysteria2,
		Address:  "sb-hy2.example.org",
		Port:     443,
		Password: "synthetic-hy2-password",
	}
}

func tuicConfig() config.Config {
	return config.Config{
		Type:     config.TypeTUIC,
		Address:  "sb-tuic.example.org",
		Port:     443,
		UUID:     "99999999-9999-9999-9999-999999999999",
		Password: "synthetic-tuic-password",
	}
}

// jsonFieldEquals checks a string field in raw JSON.
func jsonFieldEquals(raw json.RawMessage, field, want string) bool {
	if len(raw) == 0 {
		return false
	}

	var object map[string]any

	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}

	value, ok := object[field].(string)

	return ok && value == want
}
