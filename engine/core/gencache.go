package core

import (
	"fmt"
	"sync"
	"time"
)

// GenCache temporarily memoizes backend-generated runtime
// configurations for identical normalized inputs.
//
// Key (v0.9.9): fingerprint | backend | listen host | SOCKS port |
// HTTP port — everything that varies PER generated document without
// invalidating other entries (a new candidate or port change costs a
// new key, not a wholesale reset).
//
// Generation (v0.9.9): backend name | binary path | backend version —
// the inputs that change what the BACKEND produces for every
// document at once. The generation counter was documented as
// "backend-version-aware" while the hash actually covered only the
// backend name, binary path and listen addresses; the version is now
// a real input, and per-port churn no longer invalidates the entire
// cache.
//
// The cache is memory-only (never persisted), bounded and
// time-limited, because documents carry credentials: nothing is
// cached indefinitely; Clear drops everything (shutdown, cache
// maintenance).
type GenCache struct {
	mu         sync.Mutex
	entries    map[string]genEntry
	generation uint64
	maxEntries int
	ttl        time.Duration
}

type genEntry struct {
	doc       RuntimeConfig
	expiresAt time.Time
}

// DefaultGenCacheEntries bounds the cache: runtime documents are
// small but sensitive; a handful of hot configurations cover the
// connect/reconnect cycle.
const DefaultGenCacheEntries = 16

// DefaultGenCacheTTL limits how long credential-bearing documents
// stay in memory.
const DefaultGenCacheTTL = 5 * time.Minute

// NewGenCache creates a runtime generation cache.
func NewGenCache() *GenCache {
	return &GenCache{
		entries:    make(map[string]genEntry),
		maxEntries: DefaultGenCacheEntries,
		ttl:        DefaultGenCacheTTL,
	}
}

// GenerationFor folds the backend identity into the invalidation
// generation: a new backend binary (path or VERSION) invalidates
// every cached document at once. Inputs that only vary per candidate
// or per launch (listen host, ports) belong in the KEY
// (GenCacheKey), not here — changing them must not reset entries for
// other candidates.
func GenerationFor(backendName, binaryPath, backendVersion string) uint64 {
	return fnv64(backendName + "|" + binaryPath + "|" + backendVersion)
}

// Get returns a cached document for the key when the generation
// matches and the entry has not expired.
func (c *GenCache) Get(key string, generation uint64) (RuntimeConfig, bool) {
	if c == nil {
		return RuntimeConfig{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if generation != c.generation {
		// Wholesale invalidation: backend/version/settings changed.
		c.entries = make(map[string]genEntry)
		c.generation = generation

		return RuntimeConfig{}, false
	}

	entry, ok := c.entries[key]
	if !ok {
		return RuntimeConfig{}, false
	}

	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)

		return RuntimeConfig{}, false
	}

	return entry.doc, true
}

// Put stores a document under the key and generation. When the
// generation changed the cache is reset first.
func (c *GenCache) Put(key string, generation uint64, doc RuntimeConfig) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if generation != c.generation {
		c.entries = make(map[string]genEntry)
		c.generation = generation
	}

	// Bounded: evict arbitrary entries when full (documents are
	// equally cheap to regenerate).
	for len(c.entries) >= c.maxEntries {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}

	c.entries[key] = genEntry{
		doc:       doc,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Clear drops every cached document (shutdown, cache maintenance).
func (c *GenCache) Clear() {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]genEntry)
}

// Len reports the number of live entries (diagnostics).
func (c *GenCache) Len() int {
	if c == nil {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.entries)
}

// fnv64 is the FNV-1a 64-bit hash used for generation stamps.
func fnv64(data string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)

	hash := uint64(offset64)

	for i := 0; i < len(data); i++ {
		hash ^= uint64(data[i])
		hash *= prime64
	}

	return hash
}

// GenCacheKey builds the cache key from every per-document input:
// the configuration fingerprint, the backend identity and the listen
// coordinates the generated document will embed. Two launches that
// differ only in ports produce different KEYS while sharing the
// generation — no cross-candidate invalidation.
func GenCacheKey(fingerprint, backendName, localHost string, localPort, httpPort int) string {
	return fmt.Sprintf("%s|%s|%s|%d|%d",
		fingerprint, backendName, localHost, localPort, httpPort)
}
