package xray_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
)

// TestContract runs the shared backend contract suite against the
// Xray adapter with the fake-core process stand-in.
func TestContract(t *testing.T) {
	contract.Run(t, contract.Suite{
		Backend:    xray.New(),
		FakeBinary: contract.BuildFakeCore(t),
		Supported: []contract.Case{
			{Name: "vless-tcp-tls", Config: vlessTLSConfig()},
			{Name: "vless-ws", Config: vlessWSConfig()},
			{Name: "vmess-grpc", Config: vmessGRPCConfig()},
			{Name: "trojan-tcp", Config: trojanConfig()},
			{Name: "shadowsocks", Config: shadowsocksConfig()},
			{Name: "reality-vision", Config: realityVisionConfig()},
			{Name: "vless-xhttp", Config: xhttpConfig()},
			{Name: "socks", Config: socksConfig()},
			{Name: "http", Config: httpConfig()},
		},
		Unsupported: []contract.Case{
			{Name: "quic-removed-on-xray", Config: quicConfig()},
			{Name: "h2-removed-on-xray", Config: h2Config()},
			{Name: "hysteria2-unsupported", Config: hysteria2Config()},
		},
	})
}

// TestREALITYConfig verifies REALITY + vision emission in the Xray
// dialect: realitySettings, publicKey, shortId, fingerprint, flow.
func TestREALITYConfig(t *testing.T) {
	backend := xray.New()

	cfg := realityVisionConfig()

	doc, err := backend.BuildConfig(cfg, core.RuntimeOptions{
		LocalPort:       46090,
		DisableGenCache: true,
	})
	if err != nil {
		t.Fatalf("BuildConfig() = %v", err)
	}

	var parsed struct {
		Outbounds []struct {
			Settings json.RawMessage `json:"settings"`
			Stream   *struct {
				Network  string `json:"network"`
				Security string `json:"security"`
				Reality  *struct {
					ServerName  string `json:"serverName"`
					Fingerprint string `json:"fingerprint"`
					PublicKey   string `json:"publicKey"`
					ShortID     string `json:"shortId"`
				} `json:"realitySettings"`
			} `json:"streamSettings"`
		} `json:"outbounds"`
	}

	if err := json.Unmarshal(doc.Data, &parsed); err != nil {
		t.Fatalf("parse document: %v", err)
	}

	proxy := parsed.Outbounds[0]

	if proxy.Stream == nil {
		t.Fatal("no streamSettings")
	}

	if proxy.Stream.Security != "reality" {
		t.Fatalf("security = %s, want reality", proxy.Stream.Security)
	}

	if proxy.Stream.Network != "tcp" {
		t.Fatalf("network = %s, want tcp for vision", proxy.Stream.Network)
	}

	reality := proxy.Stream.Reality

	if reality == nil {
		t.Fatal("no realitySettings")
	}

	if reality.PublicKey != cfg.PublicKey {
		t.Fatalf("publicKey = %s", reality.PublicKey)
	}

	if reality.ShortID != cfg.ShortID {
		t.Fatalf("shortId = %s", reality.ShortID)
	}

	if reality.ServerName != "www.example.org" {
		t.Fatalf("serverName = %s", reality.ServerName)
	}

	var settings struct {
		VNext []struct {
			Users []struct {
				Flow string `json:"flow"`
			} `json:"users"`
		} `json:"vnext"`
	}

	if err := json.Unmarshal(proxy.Settings, &settings); err != nil {
		t.Fatalf("parse vless settings: %v", err)
	}

	if flow := settings.VNext[0].Users[0].Flow; flow != "xtls-rprx-vision" {
		t.Fatalf("flow = %q, want xtls-rprx-vision", flow)
	}

	// The summary must stay credential-free.
	for _, secret := range cfg.SecretFields() {
		if secret != "" && jsonContainsString(doc.RedactedSummary, secret) {
			t.Fatal("redacted summary leaks credential")
		}
	}
}

// TestValidateFlowRequiresTCP verifies the vision-flow transport
// constraint.
func TestValidateFlowRequiresTCP(t *testing.T) {
	backend := xray.New()

	cfg := realityVisionConfig()
	cfg.Network = "ws"

	if err := backend.Validate(context.Background(), cfg); err == nil {
		t.Fatal("vision flow over ws should be rejected")
	}
}

// TestValidateREALITYRequiresKey verifies the public-key requirement.
func TestValidateREALITYRequiresKey(t *testing.T) {
	backend := xray.New()

	cfg := realityVisionConfig()
	cfg.PublicKey = ""

	if err := backend.Validate(context.Background(), cfg); err == nil {
		t.Fatal("REALITY without a public key should be rejected")
	}
}

// TestXraySmokeRealBinary validates the Xray adapter against a real
// Xray-core binary (FREEIRAN_TEST_XRAY_BIN).
func TestXraySmokeRealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode skips real-binary smoke tests")
	}

	contract.RunSmoke(t, contract.SmokeOptions{
		BinaryPath: os.Getenv("FREEIRAN_TEST_XRAY_BIN"),
		Backend:    xray.New(),
		Configs: []config.Config{
			vlessTLSConfig(),
			vlessWSConfig(),
			vmessGRPCConfig(),
			trojanConfig(),
			shadowsocksConfig(),
			realityVisionConfig(),
			xhttpConfig(),
			socksConfig(),
			httpConfig(),
		},
		ValidateArgs: func(file string) []string {
			return []string{"run", "-test", "-c", file}
		},
		Timeout: 20 * time.Second,
	})
}

// --- fixtures (synthetic credentials only) ---

func vlessTLSConfig() config.Config {
	return config.Config{
		Type:       config.TypeVLESS,
		Address:    "x-vless.example.org",
		Port:       443,
		UUID:       "11111111-1111-1111-1111-111111111111",
		Network:    "tcp",
		Security:   "tls",
		ServerName: "x-vless.example.org",
	}
}

func vlessWSConfig() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "x-ws.example.org",
		Port:     443,
		UUID:     "22222222-2222-2222-2222-222222222222",
		Network:  "ws",
		Security: "tls",
		Path:     "/tunnel",
		Host:     "x-ws.example.org",
	}
}

func vmessGRPCConfig() config.Config {
	return config.Config{
		Type:     config.TypeVMess,
		Address:  "x-grpc.example.org",
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
		Address:  "x-trojan.example.org",
		Port:     443,
		Password: "synthetic-trojan-password",
		Network:  "tcp",
		Security: "tls",
	}
}

func shadowsocksConfig() config.Config {
	return config.Config{
		Type:     config.TypeShadowsocks,
		Address:  "x-ss.example.org",
		Port:     8388,
		Method:   "aes-256-gcm",
		Password: "synthetic-ss-password",
	}
}

func realityVisionConfig() config.Config {
	return config.Config{
		Type:               config.TypeVLESS,
		Address:            "reality.example.org",
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

func xhttpConfig() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "x-xhttp.example.org",
		Port:     443,
		UUID:     "88888888-8888-8888-8888-888888888888",
		Network:  "xhttp",
		Security: "tls",
		Path:     "/xhttp",
	}
}

func socksConfig() config.Config {
	return config.Config{
		Type:     config.TypeSOCKS,
		Address:  "x-socks.example.org",
		Port:     1080,
		Username: "synthetic-user",
		Password: "synthetic-pass",
	}
}

func httpConfig() config.Config {
	return config.Config{
		Type:     config.TypeHTTP,
		Address:  "x-http.example.org",
		Port:     8080,
		Username: "synthetic-user",
		Password: "synthetic-pass",
	}
}

func quicConfig() config.Config {
	return config.Config{
		Type:     config.TypeVMess,
		Address:  "x-quic.example.org",
		Port:     443,
		UUID:     "44444444-4444-4444-4444-444444444444",
		Network:  "quic",
		Security: "tls",
	}
}

func h2Config() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "x-h2.example.org",
		Port:     8443,
		UUID:     "55555555-5555-5555-5555-555555555555",
		Network:  "http",
		Security: "tls",
	}
}

func hysteria2Config() config.Config {
	return config.Config{
		Type:     config.TypeHysteria2,
		Address:  "x-hy2.example.org",
		Port:     443,
		Password: "synthetic-hy2-password",
	}
}

// jsonContainsString is a small substring helper.
func jsonContainsString(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
