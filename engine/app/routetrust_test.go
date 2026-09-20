package app

// routetrust_test.go pins the v0.9.8.6 route-trust boundary:
//
//   - Quick Connect / Auto never silently connects through public
//     untrusted routes (they are excluded by default and the failure
//     is EXPLICIT and user-visible);
//   - the user opt-in (AllowUntrustedPublicRoutes) re-admits public
//     routes into automatic selection;
//   - official/user-configured sources are always eligible;
//   - a public node stays fast + stable + verified reachable +
//     UNTRUSTED — reliability never promotes trust.

import (
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// qcTrustApp builds an app whose store holds exactly one candidate
// with the given route-trust stamp.
func qcTrustApp(t *testing.T, trust string) *App {
	t.Helper()

	a := newTestApp(t)

	cfg := qcConfig("node-"+trust, "99999999-9999-9999-9999-999999999999")
	cfg.SourceTrust = trust

	qcStoreConfig(t, a, cfg)

	return a
}

// TestQuickConnectExcludesUntrustedPublicRoutes pins the default
// policy: a store holding ONLY a public/untrusted candidate fails
// with the explicit untrusted-route explanation — never a silent
// connection through it.
func TestQuickConnectExcludesUntrustedPublicRoutes(t *testing.T) {
	a := qcTrustApp(t, config.SourceTrustPublic)

	_, err := a.quickConnectLoop(a.ctx, nil, 0)
	if err == nil {
		t.Fatal("Quick Connect must not silently connect through a public untrusted route")
	}

	if !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("err = %v, want the explicit untrusted-route explanation", err)
	}

	if !strings.Contains(err.Error(), "excluded") {
		t.Fatalf("err = %v, want the exclusion count surfaced", err)
	}
}

// TestQuickConnectUntrustedOptInAllowsPublicRoutes pins the opt-in:
// with AllowUntrustedPublicRoutes enabled the loop PROCEEDS past the
// trust filter (the run then fails later at the ordinary connect
// stage of this harness — no compatible core — which is the proof it
// got past the trust boundary).
func TestQuickConnectUntrustedOptInAllowsPublicRoutes(t *testing.T) {
	a := qcTrustApp(t, config.SourceTrustPublic)

	if err := a.persistSettings(func(s *Settings) {
		s.AllowUntrustedPublicRoutes = true
	}); err != nil {
		t.Fatalf("persist opt-in: %v", err)
	}

	_, err := a.quickConnectLoop(a.ctx, nil, 0)
	if err == nil {
		t.Fatal("the harness app has no runnable core, so the loop must still fail")
	}

	if strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("err = %v — the trust filter still excluded the public route despite the opt-in", err)
	}
}

// TestQuickConnectTrustedSourcesBypassFilter pins that user/official
// trust bands flow straight through the boundary.
func TestQuickConnectTrustedSourcesBypassFilter(t *testing.T) {
	for _, trust := range []string{config.SourceTrustUser, config.SourceTrustOfficial} {
		a := qcTrustApp(t, trust)

		_, err := a.quickConnectLoop(a.ctx, nil, 0)
		if err == nil {
			t.Fatalf("trust=%s: the harness app has no runnable core, so the loop must fail", trust)
		}

		if strings.Contains(err.Error(), "untrusted") {
			t.Fatalf("trust=%s: err = %v — a trusted route was excluded", trust, err)
		}
	}
}

// TestQuickConnectLegacyRecordsDefaultToUntrusted pins the
// fail-closed default: configs persisted BEFORE v0.9.8.6 (no
// source_trust field) are treated as untrusted public routes by the
// automatic policy.
func TestQuickConnectLegacyRecordsDefaultToUntrusted(t *testing.T) {
	a := qcTrustApp(t, "")

	_, err := a.quickConnectLoop(a.ctx, nil, 0)
	if err == nil {
		t.Fatal("legacy unstamped record must not be auto-connected")
	}

	if !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("err = %v, want the untrusted-route explanation for the legacy record", err)
	}
}

// TestQuickConnectMixedTrustSelectsTrusted pins the coexistence case:
// trusted and untrusted candidates both present — automatic selection
// uses the trusted one (proceeds past the filter and fails at the
// harness's connect stage instead of the trust boundary).
func TestQuickConnectMixedTrustSelectsTrusted(t *testing.T) {
	a := newTestApp(t)

	trusted := qcConfig("trusted-node", "88888888-8888-8888-8888-888888888888")
	trusted.SourceTrust = config.SourceTrustUser
	qcStoreConfig(t, a, trusted)

	untrusted := qcConfig("public-node", "77777777-7777-7777-7777-777777777777")
	untrusted.SourceTrust = config.SourceTrustPublic
	qcStoreConfig(t, a, untrusted)

	_, err := a.quickConnectLoop(a.ctx, nil, 0)
	if err == nil {
		t.Fatal("the harness app has no runnable core, so the loop must fail")
	}

	if strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("err = %v — with a trusted candidate present the trust boundary must pass", err)
	}
}
