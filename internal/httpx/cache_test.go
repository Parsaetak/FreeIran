package httpx

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMetaCacheRoundTrip verifies the persisted conditional-lookup
// cache: cold lookup misses, Put persists, a fresh instance reloads
// from disk.
func TestMetaCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()

	c, err := NewMetaCache(dir)
	if err != nil {
		t.Fatalf("NewMetaCache: %v", err)
	}

	url := "https://api.github.com/repos/example/example/releases/latest"

	if _, ok := c.Get(url); ok {
		t.Fatal("cold lookup hit, want miss")
	}

	body := []byte(`{"tag_name":"v1.2.3"}`)

	if err := c.Put(url, CachedResponse{
		URL:       url,
		ETag:      `"etag-42"`,
		Body:      body,
		FetchedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := c.Get(url)
	if !ok {
		t.Fatal("warm lookup missed after Put")
	}

	if string(got.Body) != string(body) || got.ETag != `"etag-42"` {
		t.Fatalf("cached = %+v", got)
	}

	// A new instance over the same directory must reload the entry.
	c2, err := NewMetaCache(dir)
	if err != nil {
		t.Fatalf("NewMetaCache(2): %v", err)
	}

	got2, ok := c2.Get(url)
	if !ok || string(got2.Body) != string(body) {
		t.Fatalf("persisted entry lost across reload: %+v", got2)
	}
}

// TestMetaCacheCorruptEntryDropped proves a corrupt cache file is
// dropped instead of poisoning the process.
func TestMetaCacheCorruptEntryDropped(t *testing.T) {
	dir := t.TempDir()

	// A garbage "cache entry".
	if err := os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := NewMetaCache(dir)
	if err != nil {
		t.Fatalf("NewMetaCache: %v", err)
	}

	if c.Len() != 0 {
		t.Fatalf("corrupt entries loaded: len = %d", c.Len())
	}

	// And the corrupt file was removed.
	if _, err := os.Stat(filepath.Join(dir, "garbage.json")); !os.IsNotExist(err) {
		t.Fatal("corrupt cache file not removed")
	}
}
