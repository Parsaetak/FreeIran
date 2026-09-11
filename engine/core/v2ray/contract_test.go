package v2ray_test

import (
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
)

// TestContract runs the shared backend contract suite against the
// V2Ray adapter with the fake-core process stand-in.
func TestContract(t *testing.T) {
	contract.Run(t, contract.Suite{
		Backend:    v2ray.New(),
		FakeBinary: contract.BuildFakeCore(t),
		Supported: []contract.Case{
			{Name: "vless-tcp-tls", Config: vlessTLSConfig()},
			{Name: "vless-ws", Config: vlessWSConfig()},
			{Name: "vmess-grpc", Config: vmessGRPCConfig()},
			{Name: "trojan-tcp", Config: trojanConfig()},
			{Name: "shadowsocks", Config: shadowsocksConfig()},
			{Name: "socks", Config: socksConfig()},
			{Name: "http", Config: httpConfig()},
			{Name: "vmess-quic", Config: vmessQUICConfig()},
			{Name: "vless-h2", Config: vlessH2Config()},
		},
		Unsupported: []contract.Case{
			{Name: "reality-on-v2ray", Config: realityConfig()},
			{Name: "vision-flow-on-v2ray", Config: visionFlowConfig()},
			{Name: "xhttp-on-v2ray", Config: xhttpConfig()},
			{Name: "hysteria2-unsupported", Config: hysteria2Config()},
		},
	})
}

// --- deterministic configuration fixtures (synthetic credentials) ---

func vlessTLSConfig() config.Config {
	return config.Config{
		Type:       config.TypeVLESS,
		Address:    "vless-tls.example.org",
		Port:       443,
		UUID:       "11111111-1111-1111-1111-111111111111",
		Network:    "tcp",
		Security:   "tls",
		ServerName: "vless-tls.example.org",
	}
}

func vlessWSConfig() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "vless-ws.example.org",
		Port:     443,
		UUID:     "22222222-2222-2222-2222-222222222222",
		Network:  "ws",
		Security: "tls",
		Path:     "/tunnel",
		Host:     "vless-ws.example.org",
	}
}

func vmessGRPCConfig() config.Config {
	return config.Config{
		Type:     config.TypeVMess,
		Address:  "vmess-grpc.example.org",
		Port:     443,
		UUID:     "33333333-3333-3333-3333-333333333333",
		Network:  "grpc",
		Security: "tls",
		Service:  "grpcsvc",
	}
}

func vmessQUICConfig() config.Config {
	return config.Config{
		Type:     config.TypeVMess,
		Address:  "vmess-quic.example.org",
		Port:     443,
		UUID:     "44444444-4444-4444-4444-444444444444",
		Network:  "quic",
		Security: "tls",
	}
}

func vlessH2Config() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "vless-h2.example.org",
		Port:     8443,
		UUID:     "55555555-5555-5555-5555-555555555555",
		Network:  "http",
		Security: "tls",
		Path:     "/h2",
		Host:     "vless-h2.example.org",
	}
}

func trojanConfig() config.Config {
	return config.Config{
		Type:     config.TypeTrojan,
		Address:  "trojan.example.org",
		Port:     443,
		Password: "synthetic-trojan-password",
		Network:  "tcp",
		Security: "tls",
	}
}

func shadowsocksConfig() config.Config {
	return config.Config{
		Type:     config.TypeShadowsocks,
		Address:  "ss.example.org",
		Port:     8388,
		Method:   "aes-256-gcm",
		Password: "synthetic-ss-password",
	}
}

func socksConfig() config.Config {
	return config.Config{
		Type:     config.TypeSOCKS,
		Address:  "socks.example.org",
		Port:     1080,
		Username: "synthetic-user",
		Password: "synthetic-pass",
	}
}

func httpConfig() config.Config {
	return config.Config{
		Type:     config.TypeHTTP,
		Address:  "http.example.org",
		Port:     8080,
		Username: "synthetic-user",
		Password: "synthetic-pass",
	}
}

func realityConfig() config.Config {
	return config.Config{
		Type:       config.TypeVLESS,
		Address:    "reality.example.org",
		Port:       443,
		UUID:       "66666666-6666-6666-6666-666666666666",
		Network:    "tcp",
		Security:   "reality",
		ServerName: "www.example.org",
		PublicKey:  "49gFlgsj2PdPq2SMkTD3F1U41mkAZ_QeqtAjkKi0IxY",
		ShortID:    "0123456789abcdef",
		Flow:       "xtls-rprx-vision",
	}
}

func visionFlowConfig() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "vision.example.org",
		Port:     443,
		UUID:     "77777777-7777-7777-7777-777777777777",
		Network:  "tcp",
		Security: "tls",
		Flow:     "xtls-rprx-vision",
	}
}

func xhttpConfig() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "xhttp.example.org",
		Port:     443,
		UUID:     "88888888-8888-8888-8888-888888888888",
		Network:  "xhttp",
		Security: "tls",
		Path:     "/xhttp",
	}
}

func hysteria2Config() config.Config {
	return config.Config{
		Type:     config.TypeHysteria2,
		Address:  "hy2.example.org",
		Port:     443,
		Password: "synthetic-hy2-password",
	}
}
