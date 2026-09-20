package source

import (
	"net/url"
	"strings"
	"testing"
)

func TestDefaultSources(t *testing.T) {
	sources := DefaultSources()

	if len(sources) == 0 {
		t.Fatal("expected default sources")
	}

	seen := make(map[string]struct{}, len(sources))

	for _, src := range sources {
		if src.ID == "" {
			t.Fatal("source ID must not be empty")
		}

		if src.Name == "" {
			t.Fatalf("source %q has empty name", src.ID)
		}

		if src.URL == "" {
			t.Fatalf("source %q has empty URL", src.ID)
		}

		if _, exists := seen[src.ID]; exists {
			t.Fatalf("duplicate source ID: %q", src.ID)
		}

		seen[src.ID] = struct{}{}

		parsed, err := url.Parse(src.URL)
		if err != nil {
			t.Fatalf(
				"source %q has invalid URL: %v",
				src.ID,
				err,
			)
		}

		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			t.Fatalf(
				"source %q uses unsupported URL scheme %q",
				src.ID,
				parsed.Scheme,
			)
		}

		if parsed.Host == "" {
			t.Fatalf(
				"source %q has no URL host",
				src.ID,
			)
		}
	}
}

// RequiredSources lists the configuration sources the project
// specification mandates: each must exist EXACTLY once, as a raw
// endpoint (never a GitHub /blob/ HTML page).
var RequiredSources = map[string]string{
	"scrape-and-categorize-netherlands": "https://raw.githubusercontent.com/10ium/ScrapeAndCategorize/refs/heads/main/output_configs/Netherlands.txt",
	"shadowsocks-aggregator-eternity":   "https://raw.githubusercontent.com/mahdibland/ShadowsocksAggregator/master/Eternity.txt",
	"mahsa-free-config-mtn":             "https://raw.githubusercontent.com/mahsanet/MahsaFreeConfig/main/mtn/sub_1.txt",
}

// TestRequiredSourcesExistExactlyOnce is the regression guard for the
// default source registry: every mandated source is present, unique
// by ID AND by URL, and uses its raw fetch endpoint.
func TestRequiredSourcesExistExactlyOnce(t *testing.T) {
	sources := DefaultSources()

	byID := make(map[string]int, len(sources))
	byURL := make(map[string]int, len(sources))

	for _, src := range sources {
		byID[src.ID]++
		byURL[src.URL]++
	}

	for id, wantURL := range RequiredSources {
		if n := byID[id]; n != 1 {
			t.Errorf("required source %q registered %d times, want exactly 1", id, n)
		}

		if got := countURL(sources, wantURL); got != 1 {
			t.Errorf("required URL %s registered %d times, want exactly 1", wantURL, got)
		}
	}

	// Every default URL must be a raw endpoint: raw.githubusercontent
	// hosts, never github.com /blob/ or /blame/ pages.
	for _, src := range sources {
		u, err := url.Parse(src.URL)
		if err != nil {
			t.Errorf("source %q has an invalid URL %q: %v", src.ID, src.URL, err)
			continue
		}

		if strings.EqualFold(u.Host, "github.com") {
			t.Errorf("source %q uses the GitHub HTML page %q (want the raw endpoint)", src.ID, src.URL)
		}

		if strings.Contains(u.Path, "/blob/") || strings.Contains(u.Path, "/blame/") {
			t.Errorf("source %q uses a /blob//blame/ URL %q (want the raw endpoint)", src.ID, src.URL)
		}
	}
}

func countURL(sources []Source, want string) int {
	n := 0
	for _, src := range sources {
		if src.URL == want {
			n++
		}
	}
	return n
}

// TestDefaultSourcesArePublicUntrusted pins the v0.9.8.6 route-trust
// boundary at the source registry level: every built-in public source
// is classified TrustPublic — reliable fetch behaviour and route
// trust are separate dimensions, and built-in sources are never
// silently promoted to trusted routes.
func TestDefaultSourcesArePublicUntrusted(t *testing.T) {
	defaults := DefaultSources()

	if len(defaults) == 0 {
		t.Fatal("DefaultSources is empty")
	}

	for _, src := range defaults {
		if got := src.RouteTrust(); got != TrustPublic {
			t.Errorf("source %s: RouteTrust = %q, want %q (built-in public sources are untrusted routes)",
				src.ID, got, TrustPublic)
		}

		if src.Custom {
			t.Errorf("source %s: built-in source must not be marked Custom", src.ID)
		}

		if src.Trust != TrustPublic {
			t.Errorf("source %s: Trust field = %q, want explicit %q", src.ID, src.Trust, TrustPublic)
		}
	}
}

// TestRouteTrustResolution pins the resolution rules: explicit Trust
// wins, Custom (user-added, unstamped) resolves to user trust, and
// everything else resolves to public.
func TestRouteTrustResolution(t *testing.T) {
	cases := []struct {
		name string
		src  Source
		want Trust
	}{
		{"explicit official", Source{Trust: TrustOfficial}, TrustOfficial},
		{"explicit user", Source{Trust: TrustUser}, TrustUser},
		{"explicit public", Source{Trust: TrustPublic}, TrustPublic},
		{"custom unstamped", Source{Custom: true}, TrustUser},
		{"default unstamped", Source{}, TrustPublic},
	}

	for _, tc := range cases {
		if got := tc.src.RouteTrust(); got != tc.want {
			t.Errorf("%s: RouteTrust = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDefaultSourcesExcludeRemovedNiREvilEndpoint documents the
// v0.9.8.6 correction: the nirevil-vless default pointed at the
// repository's README.md (documentation markup — zero vless://
// URIs; verified against the live repository) and the subscription
// paths it referenced 404. The entry was removed, not repaired with
// an unverified guess.
func TestDefaultSourcesExcludeRemovedNiREvilEndpoint(t *testing.T) {
	for _, src := range DefaultSources() {
		if src.ID == "nirevil-vless" {
			t.Fatal("the invalid nirevil-vless README endpoint must not be a default source")
		}

		if strings.Contains(src.URL, "README.md") {
			t.Errorf("source %s points at a README documentation endpoint (%s), not raw config data",
				src.ID, src.URL)
		}
	}
}
