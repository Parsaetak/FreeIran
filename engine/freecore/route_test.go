package freecore

import (
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func socksConfig() config.Config {
	return config.Config{
		ID:       "cfg-1",
		Type:     config.TypeSOCKS,
		Address:  "127.0.0.1",
		Port:     1080,
		Username: "u",
		Password: "p",
	}
}

func TestNormalizeAcceptsSupportedShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		out  OutboundKind
	}{
		{"socks remote", socksConfig(), OutboundSOCKS5},
		{"http remote", func() config.Config {
			c := socksConfig()
			c.Type = config.TypeHTTP

			return c
		}(), OutboundHTTP},
		{"socks remote without auth", func() config.Config {
			c := socksConfig()
			c.Username, c.Password = "", ""

			return c
		}(), OutboundSOCKS5},
		{"explicit tcp network", func() config.Config {
			c := socksConfig()
			c.Network = "tcp"

			return c
		}(), OutboundSOCKS5},
		{"explicit none security", func() config.Config {
			c := socksConfig()
			c.Security = "none"

			return c
		}(), OutboundSOCKS5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, err := Normalize(tc.cfg)
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}

			if route.Outbound != tc.out {
				t.Fatalf("outbound = %q, want %q", route.Outbound, tc.out)
			}

			if route.Endpoint.Host != "127.0.0.1" || route.Endpoint.Port != 1080 {
				t.Fatalf("endpoint = %v", route.Endpoint)
			}

			if route.Network != NetworkTCP || route.Security != SecurityNone {
				t.Fatalf("transport shape = %v/%v", route.Network, route.Security)
			}
		})
	}
}

func TestNormalizeRefusesUnsupportedShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*config.Config)
	}{
		{"vless protocol", func(c *config.Config) { c.Type = config.TypeVLESS }},
		{"vmess protocol", func(c *config.Config) { c.Type = config.TypeVMess }},
		{"trojan protocol", func(c *config.Config) { c.Type = config.TypeTrojan }},
		{"shadowsocks protocol", func(c *config.Config) { c.Type = config.TypeShadowsocks }},
		{"hysteria2 protocol", func(c *config.Config) { c.Type = config.TypeHysteria2 }},
		{"wireguard protocol", func(c *config.Config) { c.Type = config.TypeWireGuard }},
		{"websocket transport", func(c *config.Config) { c.Network = "ws" }},
		{"grpc transport", func(c *config.Config) { c.Network = "grpc" }},
		{"quic transport", func(c *config.Config) { c.Network = "quic" }},
		{"tls security", func(c *config.Config) { c.Security = "tls" }},
		{"reality security", func(c *config.Config) { c.Security = "reality" }},
		{"empty address", func(c *config.Config) { c.Address = " " }},
		{"zero port", func(c *config.Config) { c.Port = 0 }},
		{"out-of-range port", func(c *config.Config) { c.Port = 70000 }},
		{"chain", func(c *config.Config) {
			c.Chain = []*config.Config{&config.Config{Type: config.TypeSOCKS, Address: "127.0.0.1", Port: 1081}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := socksConfig()
			tc.mut(&cfg)

			if _, err := Normalize(cfg); err == nil {
				t.Fatal("Normalize must refuse the unsupported shape")
			}
		})
	}
}
