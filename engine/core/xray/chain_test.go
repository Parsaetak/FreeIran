package xray_test

import (
	"encoding/json"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
)

// chainDoc mirrors the V4 document shape for chain assertions.
type chainDoc struct {
	Inbounds  []json.RawMessage `json:"inbounds"`
	Outbounds []struct {
		Tag            string `json:"tag"`
		Protocol       string `json:"protocol"`
		StreamSettings *struct {
			Network string `json:"network"`
			Sockopt *struct {
				DialerProxy string `json:"dialerProxy"`
			} `json:"sockopt"`
		} `json:"streamSettings"`
	} `json:"outbounds"`
	Routing *struct {
		Rules []struct {
			OutboundTag string `json:"outboundTag"`
		} `json:"rules"`
	} `json:"routing"`
}

func hopA() config.Config {
	return config.Config{
		Type:    config.TypeVLESS,
		Name:    "hop-a",
		Address: "a.example.org",
		Port:    443,
		UUID:    "aaaaaaaa-1111-1111-1111-111111111111",
		Network: "tcp",
	}
}

func hopB() config.Config {
	return config.Config{
		Type:    config.TypeVLESS,
		Name:    "hop-b",
		Address: "b.example.org",
		Port:    8443,
		UUID:    "bbbbbbbb-2222-2222-2222-222222222222",
		Network: "ws",
		Path:    "/ws",
	}
}

func hopC() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Name:     "hop-c",
		Address:  "c.example.org",
		Port:     443,
		UUID:     "cccccccc-3333-3333-3333-333333333333",
		Network:  "tcp",
		Security: "tls",
	}
}

// buildChain renders a chain document through the Xray adapter
// (v0.12.2: A→B→C, C is the egress).
func buildChain(t *testing.T) chainDoc {
	t.Helper()

	exit := hopC()
	exit.Chain = []*config.Config{{
		Type:    config.TypeVLESS,
		Name:    "hop-a",
		Address: "a.example.org",
		Port:    443,
		UUID:    "aaaaaaaa-1111-1111-1111-111111111111",
		Network: "tcp",
	}, {
		Type:    config.TypeVLESS,
		Name:    "hop-b",
		Address: "b.example.org",
		Port:    8443,
		UUID:    "bbbbbbbb-2222-2222-2222-222222222222",
		Network: "ws",
		Path:    "/ws",
	}}

	backend := xray.New()

	doc, err := backend.BuildConfig(exit, core.RuntimeOptions{
		LocalPort:       45101,
		DisableGenCache: true,
	})
	if err != nil {
		t.Fatalf("BuildConfig(chain) = %v", err)
	}

	var parsed chainDoc

	if err := json.Unmarshal(doc.Data, &parsed); err != nil {
		t.Fatalf("chain document is not valid JSON: %v", err)
	}

	return parsed
}

// TestXrayChainCompilesHopsInOrder asserts the generated tag plan and
// the dialerProxy wiring: A→B→C compiles as C(dialerProxy=chain-2) →
// B(dialerProxy=chain-1) → A(direct), with C as the implicit default
// outbound ("proxy") inside ONE document.
func TestXrayChainCompilesHopsInOrder(t *testing.T) {
	doc := buildChain(t)

	if len(doc.Outbounds) != 5 { // proxy + chain-1 + chain-2 + direct + block
		t.Fatalf("outbounds = %d, want 5", len(doc.Outbounds))
	}

	// The egress hop is the FIRST outbound (V4 implicit default).
	if doc.Outbounds[0].Tag != "proxy" {
		t.Fatalf("first outbound = %q, want the egress hop tagged proxy", doc.Outbounds[0].Tag)
	}

	// Egress dials through chain-2 (hop B).
	if doc.Outbounds[0].StreamSettings == nil ||
		doc.Outbounds[0].StreamSettings.Sockopt == nil ||
		doc.Outbounds[0].StreamSettings.Sockopt.DialerProxy != "chain-2" {
		t.Fatalf("egress hop does not dial through chain-2: %+v", doc.Outbounds[0].StreamSettings)
	}

	// Second outbound = hop B (chain-2), dialing through chain-1.
	if doc.Outbounds[1].Tag != "chain-2" {
		t.Fatalf("second outbound = %q, want chain-2", doc.Outbounds[1].Tag)
	}

	if doc.Outbounds[1].StreamSettings == nil ||
		doc.Outbounds[1].StreamSettings.Sockopt == nil ||
		doc.Outbounds[1].StreamSettings.Sockopt.DialerProxy != "chain-1" {
		t.Fatalf("hop B does not dial through chain-1: %+v", doc.Outbounds[1].StreamSettings)
	}

	// Third outbound = hop A (chain-1): the FIRST hop dials directly.
	if doc.Outbounds[2].Tag != "chain-1" {
		t.Fatalf("third outbound = %q, want chain-1", doc.Outbounds[2].Tag)
	}

	if doc.Outbounds[2].StreamSettings != nil && doc.Outbounds[2].StreamSettings.Sockopt != nil {
		t.Fatalf("first hop must dial directly, got sockopt %+v", doc.Outbounds[2].StreamSettings.Sockopt)
	}

	// Auxiliaries stay out of the chain.
	if doc.Outbounds[3].Tag != "direct" || doc.Outbounds[4].Tag != "block" {
		t.Fatalf("missing direct/block outbounds: %q %q", doc.Outbounds[3].Tag, doc.Outbounds[4].Tag)
	}

	// One inbound pair at most (socks + optional http): the chain never
	// adds per-hop inbounds (never one process per hop).
	if len(doc.Inbounds) != 1 {
		t.Fatalf("inbounds = %d, want 1 (socks)", len(doc.Inbounds))
	}
}

// TestXrayChainFingerprintDistinct asserts two chains sharing an exit
// but differing in a hop never collide (runtime file names + gen
// cache keys derive from the composition fingerprint).
func TestXrayChainFingerprintDistinct(t *testing.T) {
	a := hopA()
	b := hopB()
	exit1 := hopC()
	exit1.Chain = []*config.Config{&a}
	exit2 := hopC()
	exit2.Chain = []*config.Config{&b}

	if exit1.ChainFingerprint() == exit2.ChainFingerprint() {
		t.Fatal("chains with different hops share a composition fingerprint")
	}

	if exit1.ChainFingerprint() == exit1.Fingerprint() {
		t.Fatal("chain fingerprint equals the plain exit fingerprint")
	}
}

// TestXrayChainSupportsRequiresEveryHop asserts the capability gate:
// a chain is supported only when EVERY hop matches Xray capabilities.
func TestXrayChainSupportsRequiresEveryHop(t *testing.T) {
	backend := xray.New()

	a := hopA()
	ok := hopC()
	ok.Chain = []*config.Config{&a}

	if !backend.Supports(ok) {
		t.Fatal("vless/tls + vless chain reported unsupported")
	}

	// A hop carrying REALITY stays within Xray capabilities; a hop
	// with an unsupported transport must fail the whole chain.
	bad := hopC()
	badHop := hopA()
	badHop.Network = "kcp"
	bad.Chain = []*config.Config{&badHop}

	if backend.Supports(bad) {
		t.Fatal("chain with unsupported hop transport reported supported")
	}

}
