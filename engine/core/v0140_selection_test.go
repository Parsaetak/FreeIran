package core

import (
	"context"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// v0.14.0 regression matrix — first-party capability ownership (Phase I).
// The FreeIran Engine is simulated with the same stub shape the freecore
// package's tests use; the SEMANTICS under test live in Select.
type ownershipStubCore struct {
	name  string
	owned config.Type
}

func (s *ownershipStubCore) Name() string { return s.name }

// Supports models the wide external-core surface: any transport or
// security shape of the owned protocol family.
func (s *ownershipStubCore) Supports(cfg config.Config) bool {
	return cfg.Type == s.owned
}

func (s *ownershipStubCore) Validate(_ context.Context, cfg config.Config) error { return nil }

func (s *ownershipStubCore) BuildConfig(_ config.Config, _ RuntimeOptions) (RuntimeConfig, error) {
	return RuntimeConfig{Format: "stub"}, nil
}

func (s *ownershipStubCore) Start(_ context.Context, _ config.Config, _ RuntimeOptions) (*Instance, error) {
	return nil, context.DeadlineExceeded
}

func (s *ownershipStubCore) Availability() (BackendStatus, string, string, string, string, string) {
	return StatusAvailable, "1.0.0-test", "/bin/stub-" + s.name, "", "path", "external"
}

type firstPartyStubCore struct {
	ownershipStubCore
}

// Supports pins the strict first-party shape: plain TCP, no security.
func (s *firstPartyStubCore) Supports(cfg config.Config) bool {
	networkOK := cfg.Network == "" || string(cfg.Network) == string(config.NetworkTCP)
	securityOK := cfg.Security == "" || string(cfg.Security) == string(config.SecurityNone)

	return cfg.Type == s.owned && networkOK && securityOK
}

func (s *firstPartyStubCore) Availability() (BackendStatus, string, string, string, string, string) {
	return StatusAvailable, "1.0.0-test", "", "in-process engine (compiled in; no external binary)", "builtin", "first-party"
}

func ownershipRegistry(t *testing.T) *Registry {
	t.Helper()

	registry := NewRegistry(nil)

	firstParty := &firstPartyStubCore{ownershipStubCore{name: "freecore", owned: config.TypeSOCKS}}
	external := &ownershipStubCore{name: "xray", owned: config.TypeSOCKS}

	if err := registry.Register(firstParty, 0); err != nil {
		t.Fatalf("register first-party: %v", err)
	}

	if err := registry.Register(external, 0); err != nil {
		t.Fatalf("register external: %v", err)
	}

	registry.Refresh(context.Background())

	return registry
}

func ownershipConfig(t config.Type, network, security string) config.Config {
	return config.Config{
		ID:       "ownership-it",
		Type:     t,
		Address:  "203.0.113.1",
		Port:     1080,
		Network:  network,
		Security: security,
	}
}

// TestOwnershipSOCKSRouteSelectsFirstParty: first-party-supported SOCKS
// route → freecore, with and without a preference.
func TestOwnershipSOCKSRouteSelectsFirstParty(t *testing.T) {
	registry := ownershipRegistry(t)

	sel, err := registry.Select(ownershipConfig(config.TypeSOCKS, "", ""), Preferences{})
	if err != nil {
		t.Fatalf("select: %v", err)
	}

	if sel.Core.Name() != "freecore" {
		t.Fatalf("socks route selected %q, want the first-party engine", sel.Core.Name())
	}

	sel, err = registry.Select(ownershipConfig(config.TypeSOCKS, "", ""), Preferences{PreferredBackend: "xray"})
	if err != nil {
		t.Fatalf("select with preference: %v", err)
	}

	if sel.Core.Name() != "freecore" {
		t.Fatalf("socks route + external preference selected %q, want the first-party engine", sel.Core.Name())
	}
}

// TestOwnershipHTTPRouteSelectsFirstParty: the HTTP-shape route follows
// the same ownership rule (protocol-keyed stub).
func TestOwnershipHTTPRouteSelectsFirstParty(t *testing.T) {
	registry := NewRegistry(nil)

	firstParty := &firstPartyStubCore{ownershipStubCore{name: "freecore", owned: config.TypeHTTP}}
	external := &ownershipStubCore{name: "sing-box", owned: config.TypeHTTP}

	_ = registry.Register(firstParty, 0)
	_ = registry.Register(external, 0)
	registry.Refresh(context.Background())

	sel, err := registry.Select(ownershipConfig(config.TypeHTTP, "", ""), Preferences{PreferredBackend: "sing-box"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}

	if sel.Core.Name() != "freecore" {
		t.Fatalf("http route selected %q, want the first-party engine", sel.Core.Name())
	}
}

// TestOwnershipPreferenceKeepsExternalOrdering: between external cores
// the preference still rules (an unsupported route keeps its external
// home).
func TestOwnershipPreferenceKeepsExternalOrdering(t *testing.T) {
	registry := NewRegistry(nil)

	// The first-party engine does NOT implement VLESS.
	firstParty := &firstPartyStubCore{ownershipStubCore{name: "freecore", owned: config.TypeSOCKS}}
	xray := &ownershipStubCore{name: "xray", owned: config.TypeVLESS}
	v2ray := &ownershipStubCore{name: "v2ray", owned: config.TypeVLESS}

	_ = registry.Register(firstParty, 0)
	_ = registry.Register(xray, 0)
	_ = registry.Register(v2ray, 0)
	registry.Refresh(context.Background())

	sel, err := registry.Select(ownershipConfig(config.TypeVLESS, "", ""), Preferences{PreferredBackend: "v2ray"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}

	if sel.Core.Name() != "v2ray" {
		t.Fatalf("unsupported route selected %q, want the preferred external core", sel.Core.Name())
	}
}

// TestOwnershipUnsupportedTransportGoesExternal: TLS/QUIC-shaped routes
// are beyond the first-party capability set → external core.
func TestOwnershipUnsupportedTransportGoesExternal(t *testing.T) {
	registry := NewRegistry(nil)

	firstParty := &firstPartyStubCore{ownershipStubCore{name: "freecore", owned: config.TypeSOCKS}}
	external := &ownershipStubCore{name: "sing-box", owned: config.TypeSOCKS}

	_ = registry.Register(firstParty, 0)
	_ = registry.Register(external, 2) // lower priority than 0
	registry.Refresh(context.Background())

	cases := []struct {
		network, security string
	}{
		{network: string(config.NetworkQUIC)},
		{security: string(config.SecurityTLS)},
		{security: string(config.SecurityReality)},
	}

	for _, tc := range cases {
		sel, err := registry.Select(ownershipConfig(config.TypeSOCKS, tc.network, tc.security), Preferences{})
		if err != nil {
			t.Fatalf("select %v: %v", tc, err)
		}

		if sel.Core.Name() != "sing-box" {
			t.Fatalf("route %v selected %q, want the external core", tc, sel.Core.Name())
		}
	}
}

// TestOwnershipFallbacksAreOrdered: the first-party engine leads the
// fallback chain when an external core somehow won the head (defensive:
// the reorder keeps the external core reachable on failure).
func TestOwnershipFallbacksAreOrdered(t *testing.T) {
	registry := ownershipRegistry(t)

	sel, err := registry.Select(ownershipConfig(config.TypeSOCKS, "", ""), Preferences{PreferredBackend: "xray"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}

	if len(sel.Fallbacks) != 1 || sel.Fallbacks[0] != "xray" {
		t.Fatalf("fallbacks = %v, want [xray]", sel.Fallbacks)
	}
}
