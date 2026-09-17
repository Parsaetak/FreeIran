package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CachedResponse is one persisted cache entry: the body of a
// metadata response plus its validator (ETag) and timestamp.
type CachedResponse struct {
	// URL is the origin URL the body was fetched from.
	URL string `json:"url"`

	// ETag is the response validator used for If-None-Match.
	ETag string `json:"etag,omitempty"`

	// Body is the cached response payload.
	Body []byte `json:"body"`

	// FetchedAt is when the entry was stored (UTC).
	FetchedAt time.Time `json:"fetched_at"`
}

// MetaCache is a small persisted cache for metadata responses
// (GitHub release documents). Entries are keyed by the origin URL
// (the release document is platform-independent: the caller re-parses
// the body for its platform) and hashed to a safe filename. The
// caller performs conditional requests with the stored ETag and
// re-parses the cached body on 304 Not Modified.
//
// The cache never serves stale data on network failure: a failed
// conditional request fails loudly ("release API unavailable") rather
// than silently using yesterday's release list.
type MetaCache struct {
	dir string

	mu      sync.Mutex
	entries map[string]CachedResponse
}

// NewMetaCache creates (and loads) a cache rooted at dir.
func NewMetaCache(dir string) (*MetaCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("httpx: meta cache dir: %w", err)
	}

	c := &MetaCache{
		dir:     dir,
		entries: make(map[string]CachedResponse),
	}

	c.loadAll()

	return c, nil
}

// cacheKeyFile maps a cache key to its on-disk filename.
func cacheKeyFile(key string) string {
	sum := sha256.Sum256([]byte(key))

	return hex.EncodeToString(sum[:16]) + ".json"
}

// loadAll reads every valid entry from disk. Corrupt entries are
// dropped (the cache is advisory, never authoritative).
func (c *MetaCache) loadAll() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(c.dir, entry.Name()))
		if err != nil {
			continue
		}

		var cached CachedResponse
		if err := json.Unmarshal(raw, &cached); err != nil {
			_ = os.Remove(filepath.Join(c.dir, entry.Name())) // corrupt: drop
			continue
		}

		c.entries[cacheKeyOf(cached)] = cached
	}
}

// cacheKeyOf derives the in-memory key from the entry URL. Entries
// are keyed by URL on disk (content-hashed) and by URL in memory; the
// caller's logical key (repo/channel/platform) is embedded in the URL
// itself for release lookups.
func cacheKeyOf(cached CachedResponse) string { return cached.URL }

// Get returns the cached response for key (if any).
func (c *MetaCache) Get(key string) (CachedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached, ok := c.entries[key]

	return cached, ok
}

// Put stores a response under key and persists it atomically.
func (c *MetaCache) Put(key string, cached CachedResponse) error {
	if cached.URL == "" {
		cached.URL = key
	}

	if cached.FetchedAt.IsZero() {
		cached.FetchedAt = time.Now().UTC()
	}

	c.mu.Lock()
	c.entries[key] = cached
	c.mu.Unlock()

	raw, err := json.Marshal(&cached)
	if err != nil {
		return fmt.Errorf("httpx: encode cache entry: %w", err)
	}

	tmp, err := os.CreateTemp(c.dir, "entry-*.tmp")
	if err != nil {
		return fmt.Errorf("httpx: write cache entry: %w", err)
	}

	tmpPath := tmp.Name()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return fmt.Errorf("httpx: write cache entry: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("httpx: write cache entry: %w", err)
	}

	if err := os.Rename(tmpPath, filepath.Join(c.dir, cacheKeyFile(key))); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("httpx: write cache entry: %w", err)
	}

	return nil
}

// Len reports the number of cached entries (diagnostics).
func (c *MetaCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.entries)
}
