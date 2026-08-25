package source

import (
	"net/url"
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
