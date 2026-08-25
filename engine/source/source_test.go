package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("expected User-Agent header")
		}

		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("vless://example"))
	}))
	defer server.Close()

	fetcher := NewFetcher()

	result, err := fetcher.Fetch(
		context.Background(),
		Source{
			ID:      "test-source",
			Name:    "Test Source",
			URL:     server.URL,
			Enabled: true,
		},
	)

	if err != nil {
		t.Fatalf("Fetch() failed: %v", err)
	}

	if string(result.Content) != "vless://example" {
		t.Fatalf(
			"unexpected content: %q",
			string(result.Content),
		)
	}

	if result.StatusCode != http.StatusOK {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusOK,
			result.StatusCode,
		)
	}

	if result.ContentType != "text/plain" {
		t.Fatalf(
			"expected content type %q, got %q",
			"text/plain",
			result.ContentType,
		)
	}

	if result.FetchedAt.IsZero() {
		t.Fatal("expected FetchedAt to be populated")
	}
}

func TestFetchRejectsDisabledSource(t *testing.T) {
	fetcher := NewFetcher()

	_, err := fetcher.Fetch(
		context.Background(),
		Source{
			ID:      "disabled",
			URL:     "https://example.com",
			Enabled: false,
		},
	)

	if err == nil {
		t.Fatal("expected disabled source to fail")
	}
}

func TestFetchRejectsEmptyURL(t *testing.T) {
	fetcher := NewFetcher()

	_, err := fetcher.Fetch(
		context.Background(),
		Source{
			ID:      "empty-url",
			Enabled: true,
		},
	)

	if err == nil {
		t.Fatal("expected empty URL to fail")
	}
}

func TestFetchRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	fetcher := NewFetcher()

	_, err := fetcher.Fetch(
		context.Background(),
		Source{
			ID:      "missing",
			URL:     server.URL,
			Enabled: true,
		},
	)

	if err == nil {
		t.Fatal("expected HTTP error")
	}

	if !strings.Contains(err.Error(), "404") {
		t.Fatalf(
			"expected HTTP status in error, got: %v",
			err,
		)
	}
}

func TestFetchRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer server.Close()

	fetcher := NewFetcher()
	fetcher.MaxBodySize = 5

	_, err := fetcher.Fetch(
		context.Background(),
		Source{
			ID:      "large",
			URL:     server.URL,
			Enabled: true,
		},
	)

	if err == nil {
		t.Fatal("expected oversized response to fail")
	}

	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf(
			"unexpected error: %v",
			err,
		)
	}
}
