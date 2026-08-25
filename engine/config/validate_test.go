package config

import (
	"strings"
	"testing"
)

func TestValidateAcceptsSupportedConfigurations(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			name: "VLESS",
			config: Config{
				Type:    TypeVLESS,
				Address: "example.com",
				Port:    443,
				UUID:    "test-uuid",
			},
		},
		{
			name: "VMess",
			config: Config{
				Type:    TypeVMess,
				Address: "example.com",
				Port:    443,
				UUID:    "test-uuid",
			},
		},
		{
			name: "Trojan",
			config: Config{
				Type:     TypeTrojan,
				Address:  "example.com",
				Port:     443,
				Password: "test-password",
			},
		},
		{
			name: "Shadowsocks",
			config: Config{
				Type:     TypeShadowsocks,
				Address:  "example.com",
				Port:     8388,
				Method:   "aes-256-gcm",
				Password: "test-password",
			},
		},
		{
			name: "Hysteria",
			config: Config{
				Type:     TypeHysteria,
				Address:  "example.com",
				Port:     443,
				Password: "test-password",
			},
		},
		{
			name: "Hysteria2",
			config: Config{
				Type:     TypeHysteria2,
				Address:  "example.com",
				Port:     443,
				Password: "test-password",
			},
		},
		{
			name: "TUIC",
			config: Config{
				Type:     TypeTUIC,
				Address:  "example.com",
				Port:     443,
				UUID:     "test-uuid",
				Password: "test-password",
			},
		},
		{
			name: "WireGuard",
			config: Config{
				Type:      TypeWireGuard,
				Address:   "example.com",
				Port:      51820,
				PublicKey: "test-public-key",
			},
		},
		{
			name: "SOCKS",
			config: Config{
				Type:    TypeSOCKS,
				Address: "example.com",
				Port:    1080,
			},
		},
		{
			name: "HTTP",
			config: Config{
				Type:    TypeHTTP,
				Address: "example.com",
				Port:    8080,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.config.Validate(); err != nil {
				t.Fatalf("expected valid configuration, got error: %v", err)
			}
		})
	}
}

func TestValidateRejectsInvalidBaseConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{
			name: "missing protocol",
			config: Config{
				Address: "example.com",
				Port:    443,
			},
			want: "protocol type",
		},
		{
			name: "unknown protocol",
			config: Config{
				Type:    Type("not-a-real-protocol"),
				Address: "example.com",
				Port:    443,
			},
			want: "unsupported protocol",
		},
		{
			name: "missing address",
			config: Config{
				Type: TypeVLESS,
				Port: 443,
				UUID: "test-uuid",
			},
			want: "server address",
		},
		{
			name: "whitespace address",
			config: Config{
				Type:    TypeVLESS,
				Address: "   ",
				Port:    443,
				UUID:    "test-uuid",
			},
			want: "server address",
		},
		{
			name: "zero port",
			config: Config{
				Type:    TypeVLESS,
				Address: "example.com",
				Port:    0,
				UUID:    "test-uuid",
			},
			want: "invalid port",
		},
		{
			name: "negative port",
			config: Config{
				Type:    TypeVLESS,
				Address: "example.com",
				Port:    -1,
				UUID:    "test-uuid",
			},
			want: "invalid port",
		},
		{
			name: "port above maximum",
			config: Config{
				Type:    TypeVLESS,
				Address: "example.com",
				Port:    65536,
				UUID:    "test-uuid",
			},
			want: "invalid port",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()

			if err == nil {
				t.Fatal("expected validation error, got nil")
			}

			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf(
					"expected error containing %q, got %q",
					test.want,
					err.Error(),
				)
			}
		})
	}
}

func TestValidateRejectsMissingProtocolCredentials(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{
			name: "VLESS without UUID",
			config: Config{
				Type:    TypeVLESS,
				Address: "example.com",
				Port:    443,
			},
			want: "VLESS UUID",
		},
		{
			name: "VMess without UUID",
			config: Config{
				Type:    TypeVMess,
				Address: "example.com",
				Port:    443,
			},
			want: "VMess UUID",
		},
		{
			name: "Trojan without password",
			config: Config{
				Type:    TypeTrojan,
				Address: "example.com",
				Port:    443,
			},
			want: "Trojan password",
		},
		{
			name: "Shadowsocks without method",
			config: Config{
				Type:     TypeShadowsocks,
				Address:  "example.com",
				Port:     8388,
				Password: "test-password",
			},
			want: "Shadowsocks method",
		},
		{
			name: "Shadowsocks without password",
			config: Config{
				Type:   TypeShadowsocks,
				Address: "example.com",
				Port:   8388,
				Method: "aes-256-gcm",
			},
			want: "Shadowsocks password",
		},
		{
			name: "Hysteria without authentication",
			config: Config{
				Type:    TypeHysteria,
				Address: "example.com",
				Port:    443,
			},
			want: "Hysteria authentication",
		},
		{
			name: "Hysteria2 without authentication",
			config: Config{
				Type:    TypeHysteria2,
				Address: "example.com",
				Port:    443,
			},
			want: "Hysteria authentication",
		},
		{
			name: "TUIC without UUID",
			config: Config{
				Type:     TypeTUIC,
				Address:  "example.com",
				Port:     443,
				Password: "test-password",
			},
			want: "TUIC UUID",
		},
		{
			name: "TUIC without password",
			config: Config{
				Type:    TypeTUIC,
				Address: "example.com",
				Port:    443,
				UUID:    "test-uuid",
			},
			want: "TUIC password",
		},
		{
			name: "WireGuard without public key",
			config: Config{
				Type:    TypeWireGuard,
				Address: "example.com",
				Port:    51820,
			},
			want: "WireGuard public key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()

			if err == nil {
				t.Fatal("expected validation error, got nil")
			}

			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf(
					"expected error containing %q, got %q",
					test.want,
					err.Error(),
				)
			}
		})
	}
}

func TestValidateNormalizesBeforeChecking(t *testing.T) {
	config := Config{
		Type:    " VLESS ",
		Address: " EXAMPLE.COM ",
		Port:    443,
		UUID:    "test-uuid",
	}

	if err := config.Validate(); err != nil {
		t.Fatalf("normalized valid configuration should pass: %v", err)
	}

	if config.Type != TypeVLESS {
		t.Fatalf("expected normalized type %q, got %q", TypeVLESS, config.Type)
	}

	if config.Address != "example.com" {
		t.Fatalf(
			"expected normalized address %q, got %q",
			"example.com",
			config.Address,
		)
	}
}

func TestEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		address string
		port    int
		want    string
	}{
		{
			name:    "hostname",
			address: "example.com",
			port:    443,
			want:    "example.com:443",
		},
		{
			name:    "IPv4",
			address: "192.0.2.1",
			port:    443,
			want:    "192.0.2.1:443",
		},
		{
			name:    "IPv6",
			address: "2001:db8::1",
			port:    443,
			want:    "[2001:db8::1]:443",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := Config{
				Address: test.address,
				Port:    test.port,
			}

			if got := config.Endpoint(); got != test.want {
				t.Fatalf("expected endpoint %q, got %q", test.want, got)
			}
		})
	}
}
