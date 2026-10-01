package singbox_test

import (
	"encoding/json"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/singbox"
)

// sbChainDoc mirrors the sing-box document shape for chain assertions.
type sbChainDoc struct {
	Inbounds  []json.RawMessage `json:"inbounds"`
	Outbounds []struct {
		Tag    string `json:"tag"`
		Type   string `json:"type"`
		Server string `json:"server,omitempty"`
		Detour string `json:"detour,omitempty"`
	} `json:"outbounds"`
	Route *struct {
		Final string `json:"final"`
		Rules []struct {
			IPIsPrivate bool   `json:"ip_is_private"`
			Outbound    string `json:"outbound"`
		} `json:"rules"`
	} `json:"route"`
}

// TestSingBoxChainCompilesDetourOrder asserts A→B→C compiles as
// C(detour=B) → B(detour=A) → A with route.final = "proxy" (the
// egress), inside ONE document.
func TestSingBoxChainCompilesDetourOrder(t *testing.T) {
	exit := config.Config{
		Type:     config.TypeVLESS,
		Name:     "hop-c",
		Address:  "c.example.org",
		Port:     443,
		UUID:     "cccccccc-3333-3333-3333-333333333333",
		Network:  "tcp",
		Security: "tls",
	}
	exit.Chain = []*config.Config{{
		Type:    config.TypeVLESS,
		Name:    "hop-a",
		Address: "a.example.org",
		Port:    443,
		UUID:    "aaaaaaaa-1111-1111-1111-111111111111",
		Network: "tcp",
	}, {
		Type:     config.TypeTrojan,
		Name:     "hop-b",
		Address:  "b.example.org",
		Port:     8443,
		Password: "hop-b-password",
		Network:  "tcp",
		Security: "tls",
	}}

	backend := singbox.New()

	doc, err := backend.BuildConfig(exit, core.RuntimeOptions{
		LocalPort:       45102,
		DisableGenCache: true,
	})
	if err != nil {
		t.Fatalf("BuildConfig(chain) = %v", err)
	}

	var parsed sbChainDoc

	if err := json.Unmarshal(doc.Data, &parsed); err != nil {
		t.Fatalf("chain document is not valid JSON: %v", err)
	}

	byTag := map[string]struct {
		Detour string
		Server string
		Type   string
	}{}

	for _, out := range parsed.Outbounds {
		byTag[out.Tag] = struct {
			Detour string
			Server string
			Type   string
		}{out.Detour, out.Server, out.Type}
	}

	if parsed.Route == nil || parsed.Route.Final != "proxy" {
		t.Fatalf("route final = %+v, want proxy (the egress)", parsed.Route)
	}

	egress, ok := byTag["proxy"]
	if !ok {
		t.Fatalf("no proxy outbound; tags = %+v", parsed.Outbounds)
	}

	if egress.Server != "c.example.org" {
		t.Fatalf("egress server = %q, want c.example.org", egress.Server)
	}

	if egress.Detour != "chain-2" {
		t.Fatalf("egress detour = %q, want chain-2", egress.Detour)
	}

	mid, ok := byTag["chain-2"]
	if !ok {
		t.Fatalf("no chain-2 outbound; tags = %+v", parsed.Outbounds)
	}

	if mid.Server != "b.example.org" || mid.Type != "trojan" {
		t.Fatalf("chain-2 = %+v, want hop B (trojan)", mid)
	}

	if mid.Detour != "chain-1" {
		t.Fatalf("chain-2 detour = %q, want chain-1", mid.Detour)
	}

	first, ok := byTag["chain-1"]
	if !ok {
		t.Fatalf("no chain-1 outbound; tags = %+v", parsed.Outbounds)
	}

	if first.Server != "a.example.org" {
		t.Fatalf("chain-1 server = %q, want a.example.org", first.Server)
	}

	if first.Detour != "" {
		t.Fatalf("first hop must dial directly, got detour %q", first.Detour)
	}

	// One inbound: the chain never adds per-hop inbounds.
	if len(parsed.Inbounds) != 1 {
		t.Fatalf("inbounds = %d, want 1 (mixed)", len(parsed.Inbounds))
	}
}

// TestSingBoxChainSingleHopRefused asserts the builder refuses a
// degenerate one-hop chain.
func TestSingBoxChainSingleHopRefused(t *testing.T) {
	single := config.Config{
		Type:    config.TypeVLESS,
		Name:    "only",
		Address: "a.example.org",
		Port:    443,
		UUID:    "aaaaaaaa-1111-1111-1111-111111111111",
	}

	_, _, err := singbox.BuildSingBoxChainDocument(single, core.RuntimeOptions{LocalPort: 45103})
	if err == nil {
		t.Fatal("one-hop chain accepted")
	}
}

// config.TypeTrojan guard: the constant is TypeTrojan — alias to keep
// the fixture readable above.
// (No alias needed: the fixture uses config.TypeTrojan only if defined.)
