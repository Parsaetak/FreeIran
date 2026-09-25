package singbox_test

// protocol_semantics_test.go — the v0.10.3 generated-JSON truth pass:
// the sing-box document must carry each protocol detail in its
// SING-BOX-SEMANTIC slot, derived from the model's OWN fields (the
// v0.10.2 overloaded slots are gone).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/singbox"
)

func buildRaw(t *testing.T, cfg config.Config) map[string]any {
	t.Helper()

	backend := singbox.New()

	doc, err := backend.BuildConfig(cfg, core.RuntimeOptions{
		LocalHost: "127.0.0.1",
		LocalPort: 10808,
	})
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(doc.Data, &parsed); err != nil {
		t.Fatalf("generated document is not valid JSON: %v", err)
	}

	return parsed
}

func firstArrayElement(t *testing.T, raw map[string]any, key string) map[string]any {
	t.Helper()

	items, ok := raw[key].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("document has no %s array", key)
	}

	obj, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("%s[0] is not an object", key)
	}

	return obj
}

func TestTUICDocumentCarriesCongestionControl(t *testing.T) {
	cfg := config.Config{
		Type:              config.TypeTUIC,
		Address:           "sb-tuic.example.org",
		Port:              443,
		UUID:              "99999999-9999-9999-9999-999999999999",
		Password:          "synthetic-tuic-password",
		CongestionControl: "bbr",
		UDPRelayMode:      "quic",
	}

	raw := buildRaw(t, cfg)
	outbound := firstArrayElement(t, raw, "outbounds")

	if outbound["type"] != "tuic" {
		t.Fatalf("outbound type = %v, want tuic", outbound["type"])
	}

	if outbound["congestion_control"] != "bbr" {
		t.Errorf("congestion_control = %v, want bbr", outbound["congestion_control"])
	}

	if outbound["udp_relay_mode"] != "quic" {
		t.Errorf("udp_relay_mode = %v, want quic", outbound["udp_relay_mode"])
	}

	// The transport slot must stay clean (a "network" field in the
	// outbound would mean the old overload leaked into generation).
	if _, present := outbound["network"]; present {
		t.Errorf("outbound carries a network field — the congestion_control overload leaked into generation")
	}
}

// TestTUICRelayModeAndCongestionVariants pins the FULL documented
// value domains end-to-end through generation (v0.10.4): every
// udp_relay_mode (native | quic — the v0.10.3 "quadratic" was an
// invented value) and every congestion_control (cubic | new_reno |
// bbr), plus the documented default when the URI did not choose one.
func TestTUICRelayModeAndCongestionVariants(t *testing.T) {
	for _, relay := range []string{"", "native", "quic"} {
		for _, cc := range []string{"", "cubic", "new_reno", "bbr"} {
			cfg := config.Config{
				Type:              config.TypeTUIC,
				Address:           "sb-tuic.example.org",
				Port:              443,
				UUID:              "99999999-9999-9999-9999-999999999999",
				Password:          "synthetic-tuic-password",
				CongestionControl: cc,
				UDPRelayMode:      relay,
			}

			raw := buildRaw(t, cfg)
			outbound := firstArrayElement(t, raw, "outbounds")

			wantRelay := relay
			if wantRelay == "" {
				wantRelay = "native" // sing-box documents native as the default; generation pins it
			}

			if got := outbound["udp_relay_mode"]; got != wantRelay {
				t.Errorf("udp_relay_mode = %v (input %q), want %q", got, relay, wantRelay)
			}

			// congestion_control must appear VERBATIM when chosen
			// and stay absent when not (no default invented).
			if cc == "" {
				if _, present := outbound["congestion_control"]; present {
					t.Errorf("congestion_control present for empty input — generation must not invent a default")
				}
			} else if got := outbound["congestion_control"]; got != cc {
				t.Errorf("congestion_control = %v (input %q), want %q", got, cc, cc)
			}

			// No leakage into the transport slot for ANY variant.
			if _, present := outbound["network"]; present {
				t.Errorf("udp_relay_mode=%q congestion_control=%q: outbound carries a network field", relay, cc)
			}
		}
	}
}

// TestTUICInvalidUDPRelayModeRejectedAtBuild pins the build-time
// domain enforcement for the relay mode.
func TestTUICInvalidUDPRelayModeRejectedAtBuild(t *testing.T) {
	backend := singbox.New()

	_, err := backend.BuildConfig(config.Config{
		Type:         config.TypeTUIC,
		Address:      "sb-tuic.example.org",
		Port:         443,
		UUID:         "99999999-9999-9999-9999-999999999999",
		Password:     "synthetic-tuic-password",
		UDPRelayMode: "quadratic", // the invented v0.10.3 value
	}, core.RuntimeOptions{LocalHost: "127.0.0.1", LocalPort: 10808})

	if err == nil {
		t.Fatal("BuildConfig accepted udp_relay_mode=quadratic")
	}

	if !strings.Contains(err.Error(), "udp_relay_mode") {
		t.Fatalf("error = %v, want a udp_relay_mode message", err)
	}
}

func TestTUICInvalidCongestionControlRejectedAtBuild(t *testing.T) {
	backend := singbox.New()

	_, err := backend.BuildConfig(config.Config{
		Type:              config.TypeTUIC,
		Address:           "sb-tuic.example.org",
		Port:              443,
		UUID:              "99999999-9999-9999-9999-999999999999",
		Password:          "synthetic-tuic-password",
		CongestionControl: "vegas",
	}, core.RuntimeOptions{LocalHost: "127.0.0.1", LocalPort: 10808})

	if err == nil {
		t.Fatal("BuildConfig accepted congestion_control=vegas")
	}

	if !strings.Contains(err.Error(), "congestion_control") {
		t.Fatalf("error = %v, want a congestion_control message", err)
	}
}

func TestHysteria2DocumentCarriesObfsObject(t *testing.T) {
	// Both documented obfs types (v0.10.4: gecko added — v0.10.3
	// only knew salamander). The generated shape MUST be the
	// sing-box OBJECT {"type", "password"} for both.
	for _, obfsType := range []string{"salamander", "gecko"} {
		cfg := config.Config{
			Type:         config.TypeHysteria2,
			Address:      "sb-hy2.example.org",
			Port:         443,
			Password:     "synthetic-hy2-password",
			Obfs:         obfsType,
			ObfsPassword: "synthetic-obfs-password",
		}

		raw := buildRaw(t, cfg)
		outbound := firstArrayElement(t, raw, "outbounds")

		obfs, ok := outbound["obfs"].(map[string]any)
		if !ok {
			t.Fatalf("obfs=%s: outbound.obfs missing or not an object: %v", obfsType, outbound["obfs"])
		}

		if obfs["type"] != obfsType {
			t.Errorf("obfs=%s: obfs.type = %v", obfsType, obfs["type"])
		}

		if obfs["password"] != "synthetic-obfs-password" {
			t.Errorf("obfs=%s: obfs.password = %v, want the obfs-password field value", obfsType, obfs["password"])
		}

		// The Hysteria2 obfs must never leak into the transport,
		// security or host slots.
		if _, present := outbound["network"]; present {
			t.Errorf("obfs=%s: outbound carries a network field", obfsType)
		}
	}
}

// TestHysteria2InvalidObfsRejectedAtBuild pins the build-time domain
// enforcement of the obfs type.
func TestHysteria2InvalidObfsRejectedAtBuild(t *testing.T) {
	backend := singbox.New()

	_, err := backend.BuildConfig(config.Config{
		Type:     config.TypeHysteria2,
		Address:  "sb-hy2.example.org",
		Port:     443,
		Password: "synthetic-hy2-password",
		Obfs:     "not-a-real-obfs",
	}, core.RuntimeOptions{LocalHost: "127.0.0.1", LocalPort: 10808})

	if err == nil {
		t.Fatal("BuildConfig accepted obfs=not-a-real-obfs")
	}

	if !strings.Contains(err.Error(), "obfs") {
		t.Fatalf("error = %v, want an obfs message", err)
	}
}

// TestHysteriaV1ObfsJSONString pins the EXACT JSON type of the
// Hysteria (v1) obfs field (v0.10.4): the v1 schema takes a plain
// STRING (the obfuscation password) — v0.10.3 emitted the
// Hysteria2-style object here because the protocols share a name,
// and the real binary rejects that shape for hysteria.
func TestHysteriaV1ObfsJSONString(t *testing.T) {
	// Present → JSON string, verbatim.
	cfg := config.Config{
		Type:     config.TypeHysteria,
		Address:  "sb-hy.example.org",
		Port:     443,
		Password: "synthetic-hy-password",
		Obfs:     "synthetic-obfs-password",
		UpMbps:   100,
		DownMbps: 500,
	}

	raw := buildRaw(t, cfg)
	outbound := firstArrayElement(t, raw, "outbounds")

	obfs, ok := outbound["obfs"].(string)
	if !ok {
		t.Fatalf("outbound.obfs is not a JSON string: %v (type %T) — Hysteria v1 must never inherit the Hysteria2 object shape", outbound["obfs"], outbound["obfs"])
	}

	if obfs != "synthetic-obfs-password" {
		t.Errorf("outbound.obfs = %q, want the obfs password verbatim", obfs)
	}

	// Absent → the key is omitted entirely.
	cfg.Obfs = ""
	raw = buildRaw(t, cfg)
	outbound = firstArrayElement(t, raw, "outbounds")

	if _, present := outbound["obfs"]; present {
		t.Errorf("outbound carries obfs %v for an empty Obfs field — must be omitted", outbound["obfs"])
	}
}

func TestWireGuardEndpointCarriesLocalAddressAndPeerList(t *testing.T) {
	cfg := wireGuardConfig()
	cfg.InterfaceAddress = []string{"10.7.0.2/32"}

	raw := buildRaw(t, cfg)

	endpoints, ok := raw["endpoints"].([]any)
	if !ok || len(endpoints) == 0 {
		t.Fatalf("document has no endpoints array (WireGuard must ride the v1.11+ endpoint form)")
	}

	endpoint, ok := endpoints[0].(map[string]any)
	if !ok {
		t.Fatalf("endpoints[0] is not an object")
	}

	// The LOCAL interface address — the real v1.14 binary rejects an
	// endpoint without it ("missing local address").
	address, ok := endpoint["address"].([]any)
	if !ok || len(address) == 0 {
		t.Fatalf("endpoint.address missing — the generated endpoint cannot start on the real binary")
	}

	if address[0] != "10.7.0.2/32" {
		t.Errorf("endpoint.address[0] = %v, want 10.7.0.2/32", address[0])
	}

	peers, ok := endpoint["peers"].([]any)
	if !ok || len(peers) == 0 {
		t.Fatalf("endpoint.peers missing")
	}

	peer, ok := peers[0].(map[string]any)
	if !ok {
		t.Fatalf("peers[0] is not an object")
	}

	if peer["address"] != "192.0.2.1" {
		t.Errorf("peers[0].address = %v, want the PEER endpoint 192.0.2.1", peer["address"])
	}

	allowedIPs, ok := peer["allowed_ips"].([]any)
	if !ok || len(allowedIPs) == 0 {
		t.Fatalf("peers[0].allowed_ips missing")
	}

	if allowedIPs[0] != "0.0.0.0/0" {
		t.Errorf("peers[0].allowed_ips[0] = %v, want 0.0.0.0/0 (the peer routing list)", allowedIPs[0])
	}
}

func TestWireGuardEndpointGeneratesDeterministicLocalAddress(t *testing.T) {
	// The v0.10.2 document had NO local address; the v0.10.3
	// generator supplies a deterministic FreeIran-generated fallback
	// pair (NOT a sing-box documented default — sing-box only
	// requires some local address) so the endpoint reaches startup
	// on the real binary even when the imported configuration did
	// not record an Address.
	cfg := wireGuardConfig()

	raw := buildRaw(t, cfg)

	endpoints, ok := raw["endpoints"].([]any)
	if !ok || len(endpoints) == 0 {
		t.Fatalf("document has no endpoints array")
	}

	endpoint, ok := endpoints[0].(map[string]any)
	if !ok {
		t.Fatalf("endpoints[0] is not an object")
	}

	address, ok := endpoint["address"].([]any)
	if !ok || len(address) == 0 {
		t.Fatalf("endpoint.address missing — the real binary would reject this endpoint at startup")
	}
}
