package config

import "testing"

func TestConfigFingerprintIsDeterministic(t *testing.T) {
	config := Config{
		Type:       TypeVLESS,
		Address:    "example.com",
		Port:       443,
		UUID:       "12345678-1234-1234-1234-123456789abc",
		Network:    "ws",
		Path:       "/api",
		Host:       "example.com",
		Security:   "tls",
		ServerName: "example.com",
	}

	first := config.Fingerprint()
	second := config.Fingerprint()

	if first == "" {
		t.Fatal("fingerprint must not be empty")
	}

	if first != second {
		t.Fatalf("fingerprint is not deterministic: %q != %q", first, second)
	}
}

func TestIdenticalConfigsHaveSameFingerprint(t *testing.T) {
	first := Config{
		Type:     TypeVLESS,
		Address:  "example.com",
		Port:     443,
		UUID:     "test-uuid",
		Network:  "ws",
		Path:     "/vpn",
		Security: "tls",
	}

	second := first

	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("identical configurations must have the same fingerprint")
	}
}

func TestRuntimeFieldsDoNotChangeFingerprint(t *testing.T) {
	first := Config{
		Type:       TypeVLESS,
		Address:    "example.com",
		Port:       443,
		UUID:       "test-uuid",
		Network:    "ws",
		Path:       "/vpn",
		Security:   "tls",
		Source:     "source-a",
		Working:    false,
		LatencyMS:  500,
		TestedAt:   1000,
	}

	second := first
	second.Source = "source-b"
	second.Working = true
	second.LatencyMS = 100
	second.TestedAt = 2000

	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("runtime and source metadata must not affect the fingerprint")
	}
}

func TestMeaningfulConfigChangeChangesFingerprint(t *testing.T) {
	base := Config{
		Type:     TypeVLESS,
		Address:  "example.com",
		Port:     443,
		UUID:     "test-uuid",
		Network:  "ws",
		Path:     "/vpn",
		Security: "tls",
	}

	tests := []struct {
		name   string
		change func(*Config)
	}{
		{
			name: "protocol",
			change: func(c *Config) {
				c.Type = TypeVMess
			},
		},
		{
			name: "address",
			change: func(c *Config) {
				c.Address = "other.example.com"
			},
		},
		{
			name: "port",
			change: func(c *Config) {
				c.Port = 8443
			},
		},
		{
			name: "identity",
			change: func(c *Config) {
				c.UUID = "different-uuid"
			},
		},
		{
			name: "network",
			change: func(c *Config) {
				c.Network = "grpc"
			},
		},
		{
			name: "path",
			change: func(c *Config) {
				c.Path = "/different"
			},
		},
		{
			name: "security",
			change: func(c *Config) {
				c.Security = "reality"
			},
		},
	}

	baseFingerprint := base.Fingerprint()

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			test.change(&changed)

			if baseFingerprint == changed.Fingerprint() {
				t.Fatalf(
					"meaningful %s change must produce a different fingerprint",
					test.name,
				)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	config := Config{
		Type:       " VLESS ",
		Address:    " EXAMPLE.COM ",
		Name:       " Test Server ",
		Network:    " WS ",
		Security:   " TLS ",
		ServerName: " example.com ",
		Host:       " example.com ",
		Path:       " /vpn ",
		Service:    " service ",
		Method:     " AES-128-GCM ",
	}

	config.Normalize()

	if config.Type != TypeVLESS {
		t.Errorf("expected type %q, got %q", TypeVLESS, config.Type)
	}

	if config.Address != "example.com" {
		t.Errorf("expected normalized address %q, got %q", "example.com", config.Address)
	}

	if config.Name != "Test Server" {
		t.Errorf("expected normalized name %q, got %q", "Test Server", config.Name)
	}

	if config.Network != "ws" {
		t.Errorf("expected network %q, got %q", "ws", config.Network)
	}

	if config.Security != "tls" {
		t.Errorf("expected security %q, got %q", "tls", config.Security)
	}

	if config.ServerName != "example.com" {
		t.Errorf("expected server name %q, got %q", "example.com", config.ServerName)
	}

	if config.Host != "example.com" {
		t.Errorf("expected host %q, got %q", "example.com", config.Host)
	}

	if config.Path != "/vpn" {
		t.Errorf("expected path %q, got %q", "/vpn", config.Path)
	}

	if config.Service != "service" {
		t.Errorf("expected service %q, got %q", "service", config.Service)
	}

	if config.Method != "aes-128-gcm" {
		t.Errorf("expected method %q, got %q", "aes-128-gcm", config.Method)
	}
}

func TestSetID(t *testing.T) {
	config := Config{
		Type:    TypeVLESS,
		Address: "example.com",
		Port:    443,
		UUID:    "test-uuid",
	}

	if config.ID != "" {
		t.Fatal("ID should initially be empty")
	}

	config.SetID()

	if config.ID == "" {
		t.Fatal("SetID must assign an ID")
	}

	if config.ID != config.Fingerprint() {
		t.Fatal("ID must equal the configuration fingerprint")
	}
}

func TestDifferentProtocolsDoNotCollide(t *testing.T) {
	vless := Config{
		Type:     TypeVLESS,
		Address:  "example.com",
		Port:     443,
		UUID:     "test",
	}

	vmess := vless
	vmess.Type = TypeVMess

	trojan := vless
	trojan.Type = TypeTrojan

	if vless.Fingerprint() == vmess.Fingerprint() {
		t.Fatal("VLESS and VMess fingerprints must differ")
	}

	if vless.Fingerprint() == trojan.Fingerprint() {
		t.Fatal("VLESS and Trojan fingerprints must differ")
	}

	if vmess.Fingerprint() == trojan.Fingerprint() {
		t.Fatal("VMess and Trojan fingerprints must differ")
	}
}
