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
		UDPRelayMode:      "quadratic",
	}

	raw := buildRaw(t, cfg)
	outbound := firstArrayElement(t, raw, "outbounds")

	if outbound["type"] != "tuic" {
		t.Fatalf("outbound type = %v, want tuic", outbound["type"])
	}

	if outbound["congestion_control"] != "bbr" {
		t.Errorf("congestion_control = %v, want bbr", outbound["congestion_control"])
	}

	if outbound["udp_relay_mode"] != "quadratic" {
		t.Errorf("udp_relay_mode = %v, want quadratic", outbound["udp_relay_mode"])
	}

	// The transport slot must stay clean (a "network" field in the
	// outbound would mean the old overload leaked into generation).
	if _, present := outbound["network"]; present {
		t.Errorf("outbound carries a network field — the congestion_control overload leaked into generation")
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
	cfg := config.Config{
		Type:         config.TypeHysteria2,
		Address:      "sb-hy2.example.org",
		Port:         443,
		Password:     "synthetic-hy2-password",
		Obfs:         "salamander",
		ObfsPassword: "synthetic-obfs-password",
	}

	raw := buildRaw(t, cfg)
	outbound := firstArrayElement(t, raw, "outbounds")

	obfs, ok := outbound["obfs"].(map[string]any)
	if !ok {
		t.Fatalf("outbound.obfs missing or not an object: %v", outbound["obfs"])
	}

	if obfs["type"] != "salamander" {
		t.Errorf("obfs.type = %v, want salamander", obfs["type"])
	}

	if obfs["password"] != "synthetic-obfs-password" {
		t.Errorf("obfs.password = %v, want the obfs-password field value", obfs["password"])
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
	// generator provides the documented deterministic default so the
	// endpoint reaches startup on the real binary even when the
	// imported configuration did not record an Address.
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
