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
func TestWireGuardFingerprintIncludesMeaningfulFields(t *testing.T) {
	base := Config{
		Type:       TypeWireGuard,
		Address:    "example.com",
		Port:       51820,
		PublicKey:  "peer-public-key",
		AllowedIPs: []string{"0.0.0.0/0"},
		DNS:        []string{"1.1.1.1"},
	}

	tests := []struct {
		name   string
		change func(*Config)
	}{
		{
			name: "public key",
			change: func(c *Config) {
				c.PublicKey = "different-public-key"
			},
		},
		{
			name: "allowed IPs",
			change: func(c *Config) {
				c.AllowedIPs = []string{"10.0.0.0/8"}
			},
		},
		{
			name: "DNS",
			change: func(c *Config) {
				c.DNS = []string{"8.8.8.8"}
			},
		},
		{
			name: "MTU",
			change: func(c *Config) {
				c.MTU = 1420
			},
		},
		{
			name: "persistent keepalive",
			change: func(c *Config) {
				c.PersistentKeepalive = 25
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

func TestWireGuardPrivateKeyDoesNotChangeFingerprint(t *testing.T) {
	first := Config{
		Type:       TypeWireGuard,
		Address:    "example.com",
		Port:       51820,
		PublicKey:  "peer-public-key",
		PrivateKey: "private-key-a",
		AllowedIPs: []string{"0.0.0.0/0"},
	}

	second := first
	second.PrivateKey = "private-key-b"

	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("private key must not affect configuration fingerprint")
	}
}

func TestWireGuardNormalize(t *testing.T) {
	config := Config{
		Type: TypeWireGuard,
		AllowedIPs: []string{
			"0.0.0.0/0",
			" 0.0.0.0/0 ",
			"10.0.0.0/8",
			"",
		},
		DNS: []string{
			"1.1.1.1",
			" 1.1.1.1 ",
			"",
		},
	}

	config.Normalize()

	if len(config.AllowedIPs) != 2 {
		t.Fatalf(
			"expected 2 unique allowed IPs, got %d",
			len(config.AllowedIPs),
		)
	}

	if config.AllowedIPs[0] != "0.0.0.0/0" {
		t.Fatalf(
			"unexpected first allowed IP: %q",
			config.AllowedIPs[0],
		)
	}

	if config.AllowedIPs[1] != "10.0.0.0/8" {
		t.Fatalf(
			"unexpected second allowed IP: %q",
			config.AllowedIPs[1],
		)
	}

	if len(config.DNS) != 1 {
		t.Fatalf(
			"expected 1 unique DNS server, got %d",
			len(config.DNS),
		)
	}

	if config.DNS[0] != "1.1.1.1" {
		t.Fatalf(
			"unexpected DNS value: %q",
			config.DNS[0],
		)
	}
}
