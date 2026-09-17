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
