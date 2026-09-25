package parser

// protocol_semantics_test.go — the v0.10.3 protocol data-model truth
// pass. Each enumerated protocol detail must land in its OWN semantic
// field (never an overloaded slot), and values outside the documented
// domain must be rejected by validation instead of reaching a core.
//
// v0.10.2 defects pinned here:
//
//   - TUIC congestion_control was written into Network, so the
//     capability matcher read "bbr"/"cubic"/"new_reno" as TRANSPORT
//     names and every such config failed backend matching.
//   - TUIC udp_relay_mode was dropped.
//   - Hysteria obfs was written into Security and obfs-password into
//     Host (the WS host-header slot), corrupting both.
//   - The WireGuard INI "Address" (the LOCAL interface address) was
//     dropped, so the generated endpoint had no local address.

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func TestTUICCongestionControlOwnField(t *testing.T) {
	p := New()

	input := "tuic://uuid-goes-here:pw@example.com:443" +
		"?congestion_control=bbr&udp_relay_mode=quic&sni=example.com"

	cfgs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(cfgs) != 1 {
		t.Fatalf("configs = %d, want 1", len(cfgs))
	}

	cfg := cfgs[0]

	if cfg.CongestionControl != "bbr" {
		t.Errorf("CongestionControl = %q, want bbr", cfg.CongestionControl)
	}

	if cfg.UDPRelayMode != "quic" {
		t.Errorf("UDPRelayMode = %q, want quic", cfg.UDPRelayMode)
	}

	// THE regression: Network is the TRANSPORT slot. TUIC is
	// QUIC-based and carries no transport; a congestion-control
	// algorithm must never leak into it (the capability matcher
	// would read it as a transport name).
	if cfg.Network != "" {
		t.Errorf("Network = %q, want empty (congestion_control must not overload the transport slot)", cfg.Network)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestTUICInvalidCongestionControlRejected(t *testing.T) {
	p := New()

	// Rejected at PARSE time (the parser runs config.Validate on
	// every candidate) — values outside the documented domain never
	// become configurations.
	_, err := p.Parse([]byte(
		"tuic://uuid:pw@example.com:443?congestion_control=vegas"))
	if err == nil {
		t.Fatal("Parse accepted congestion_control=vegas — values outside the documented domain must be rejected")
	}

	if !strings.Contains(err.Error(), "congestion_control") {
		t.Fatalf("error = %v, want a congestion_control message", err)
	}
}

func TestTUICInvalidUDPRelayModeRejected(t *testing.T) {
	p := New()

	_, err := p.Parse([]byte(
		"tuic://uuid:pw@example.com:443?udp_relay_mode=ratelimited"))
	if err == nil {
		t.Fatal("Parse accepted udp_relay_mode=ratelimited")
	}

	if !strings.Contains(err.Error(), "udp_relay_mode") {
		t.Fatalf("error = %v, want a udp_relay_mode message", err)
	}
}

func TestHysteriaObfsOwnFields(t *testing.T) {
	p := New()

	// Hysteria2: obfs is the obfuscation TYPE (salamander | gecko
	// for sing-box v1.14 — v0.10.4 added the documented gecko which
	// v0.10.3 rejected) and obfs-password carries the password.
	for _, obfsType := range []string{"salamander", "gecko"} {
		input := "hysteria2://auth-password@example.com:443" +
			"?obfs=" + obfsType + "&obfs-password=obfs-secret&sni=example.com&insecure=1"

		cfgs, err := p.Parse([]byte(input))
		if err != nil {
			t.Fatalf("Parse(hysteria2, obfs=%s): %v", obfsType, err)
		}

		if len(cfgs) != 1 {
			t.Fatalf("hysteria2: configs = %d, want 1", len(cfgs))
		}

		cfg := cfgs[0]

		if cfg.Obfs != obfsType {
			t.Errorf("hysteria2: Obfs = %q, want %s", cfg.Obfs, obfsType)
		}

		if cfg.ObfsPassword != "obfs-secret" {
			t.Errorf("hysteria2: ObfsPassword = %q, want obfs-secret", cfg.ObfsPassword)
		}

		// The overloaded slots must stay CLEAN: Security is the TLS
		// layer, Host is the WS/H2 host header — neither may carry
		// obfuscation values.
		if cfg.Security != "" {
			t.Errorf("hysteria2: Security = %q, want empty (obfs must not overload the security slot)", cfg.Security)
		}

		if cfg.Host != "" {
			t.Errorf("hysteria2: Host = %q, want empty (obfs-password must not overload the host slot)", cfg.Host)
		}

		if !cfg.Insecure {
			t.Errorf("hysteria2: Insecure = false, want true")
		}

		if err := cfg.Validate(); err != nil {
			t.Errorf("hysteria2: Validate: %v", err)
		}
	}
}

// TestHysteriaV1ObfsIsThePasswordString pins the v1 URI semantics:
// the Hysteria (v1) format has NO obfs type concept — the `obfs`
// parameter IS the obfuscation password the server was configured
// with (v0.10.4; v0.10.3 forced it through the v2 salamander enum and
// rejected every real v1 URI whose obfs password was not literally
// "salamander").
func TestHysteriaV1ObfsIsThePasswordString(t *testing.T) {
	p := New()

	input := "hysteria://auth-password@example.com:443" +
		"?obfs=xplus-obfs-secret&upmbps=100&downmbps=500&insecure=1"

	cfgs, err := p.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse(hysteria): %v", err)
	}

	if len(cfgs) != 1 {
		t.Fatalf("configs = %d, want 1", len(cfgs))
	}

	cfg := cfgs[0]

	if cfg.Type != config.TypeHysteria {
		t.Fatalf("Type = %v, want hysteria", cfg.Type)
	}

	if cfg.Obfs != "xplus-obfs-secret" {
		t.Errorf("Obfs = %q, want the v1 obfs password verbatim", cfg.Obfs)
	}

	if cfg.Security != "" || cfg.Host != "" {
		t.Errorf("Security = %q, Host = %q — both slots must stay clean", cfg.Security, cfg.Host)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestHysteriaInvalidObfsRejected(t *testing.T) {
	p := New()

	// Rejection happens at PARSE time (the parser runs
	// config.Validate on every candidate) — an obfuscation value
	// outside the documented domain never becomes a configuration,
	// instead of being passed through to the core.
	_, err := p.Parse([]byte(
		"hysteria2://pw@example.com:443?obfs=not-a-real-obfs"))
	if err == nil {
		t.Fatal("Parse accepted obfs=not-a-real-obfs — obfuscation values must be validated, not passed through")
	}

	if !strings.Contains(err.Error(), "obfs") {
		t.Fatalf("error = %v, want an obfs message", err)
	}

	// The v1 obfs is a password string, NOT the v2 enum: the same
	// value that is invalid as a v2 obfs type is a legitimate v1
	// obfuscation password.
	cfgs, err := p.Parse([]byte(
		"hysteria://pw@example.com:443?obfs=not-a-real-obfs&upmbps=100&downmbps=500"))
	if err != nil {
		t.Fatalf("Parse(hysteria v1, obfs=not-a-real-obfs): %v", err)
	}

	if cfgs[0].Obfs != "not-a-real-obfs" {
		t.Errorf("v1 Obfs = %q, want the password verbatim", cfgs[0].Obfs)
	}
}

func TestWireGuardINIInterfaceAddress(t *testing.T) {
	p := New()

	ini := "[Interface]\n" +
		"PrivateKey = " + deterministicINIKey(0x21) + "\n" +
		"Address = 10.7.0.2/32, fd00::2/128\n" +
		"DNS = 1.1.1.1\n\n" +
		"[Peer]\n" +
		"PublicKey = " + deterministicINIKey(0x42) + "\n" +
		"Endpoint = 192.0.2.10:51820\n" +
		"AllowedIPs = 0.0.0.0/0\n"

	cfgs, err := p.Parse([]byte(ini))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(cfgs) != 1 {
		t.Fatalf("configs = %d, want 1", len(cfgs))
	}

	cfg := cfgs[0]

	if len(cfg.InterfaceAddress) != 2 ||
		cfg.InterfaceAddress[0] != "10.7.0.2/32" ||
		cfg.InterfaceAddress[1] != "fd00::2/128" {
		t.Errorf("InterfaceAddress = %v, want [10.7.0.2/32 fd00::2/128] (the INI Address is the LOCAL interface address)", cfg.InterfaceAddress)
	}

	// AllowedIPs is the PEER list — it must carry exactly what the
	// peer section declared, never the interface address.
	if len(cfg.AllowedIPs) != 1 || cfg.AllowedIPs[0] != "0.0.0.0/0" {
		t.Errorf("AllowedIPs = %v, want [0.0.0.0/0] (the peer routing list)", cfg.AllowedIPs)
	}
}

func TestWireGuardURLInterfaceAddress(t *testing.T) {
	p := New()

	// Both URI spellings must stay reachable (§6).
	for _, scheme := range []string{"wireguard", "wg"} {
		input := scheme + "://" + deterministicINIKey(0x42) +
			"@192.0.2.10:51820?privateKey=" + deterministicINIKey(0x21) +
			"&address=10.7.0.2%2F32&allowedIPs=0.0.0.0%2F0"

		cfgs, err := p.Parse([]byte(input))
		if err != nil {
			t.Fatalf("Parse(%s): %v", scheme, err)
		}

		if len(cfgs) != 1 {
			t.Fatalf("%s: configs = %d, want 1", scheme, len(cfgs))
		}

		cfg := cfgs[0]

		if cfg.Type != config.TypeWireGuard {
			t.Errorf("%s: Type = %v, want wireguard", scheme, cfg.Type)
		}

		if len(cfg.InterfaceAddress) != 1 || cfg.InterfaceAddress[0] != "10.7.0.2/32" {
			t.Errorf("%s: InterfaceAddress = %v, want [10.7.0.2/32] (the LOCAL interface address)", scheme, cfg.InterfaceAddress)
		}

		if len(cfg.AllowedIPs) != 1 || cfg.AllowedIPs[0] != "0.0.0.0/0" {
			t.Errorf("%s: AllowedIPs = %v, want [0.0.0.0/0] (the PEER routing list)", scheme, cfg.AllowedIPs)
		}
	}
}

// TestCapabilityMatcherNeverSeesCongestionValues is the
// capability-matcher guard for the §5 requirement: a TUIC config
// parsed from a real URI must match the sing-box capability set —
// the matcher must not interpret "bbr"/"cubic"/"new_reno" as
// transport names. (The model-level fix is that CongestionControl
// never enters Network; this test pins the end-to-end behavior.)
func TestCapabilityMatcherNeverSeesCongestionValues(t *testing.T) {
	p := New()

	cfgs, err := p.Parse([]byte(
		"tuic://uuid:pw@example.com:443?congestion_control=cubic"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cfg := cfgs[0]
	cfg.Normalize()

	for _, value := range config.TUICCongestionControlValues {
		if strings.EqualFold(cfg.Network, value) {
			t.Fatalf("Network carried the congestion value %q — the capability matcher would read it as a transport", value)
		}
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// deterministicINIKey generates the synthetic WireGuard key material
// at runtime (the Gitleaks contract: no key-shaped literals
// committed).
func deterministicINIKey(seed byte) string {
	key := make([]byte, 32)

	for i := range key {
		key[i] = seed + byte(i)
	}

	return base64.StdEncoding.EncodeToString(key)
}
