// Package cache provides the FreeIran multi-layer cache architecture.
//
// Layers (highest to lowest):
//
//	HOT CONFIGURATION CACHE  decoded config objects used by the UI
//	NORMALIZED DATA CACHE    normalized records kept between stages
//	INDEX CACHE              chunk/index lookups inside the store
//	PARSE CACHE              parsed source payloads keyed by content hash
//	SOURCE CACHE             fetched raw payloads keyed by URL + hash
//
// This package exposes one generic LRU implementation with:
//
//   - entry-count and approximate-byte size bounds,
//   - optional per-entry TTL,
//   - schema/version stamps so stale generations are never returned,
//   - hit/miss statistics,
//   - deterministic LRU eviction.
//
// Every layer is bounded: no cache here can grow without limit.
package cache

import (
	"container/list"
	"sync"
	"time"
)

// Options configures a cache layer.
type Options struct {
	// MaxEntries bounds the number of live entries. 0 = unlimited
	// (not recommended; used only by tiny internal layers).
	MaxEntries int

	// MaxBytes approximates the memory budget. Weigh must be set for
	// it to take effect. 0 = unlimited.
	MaxBytes int64

	// Weigh approximates the memory cost of a value in bytes.
	Weigh func(value any) int64

	// TTL expires entries older than the duration. 0 = no expiry.
	TTL time.Duration

	// OnEvict, when set, is invoked with the stored value for every
	// entry removed from the cache (LRU eviction, expiry, stale
	// generation, Invalidate and Clear). Use it to release resources
	// owned by cached values — a cache must never silently forget a
	// native resource it owns.
	OnEvict func(value any)

	// Name identifies the layer in diagnostics.
	Name string
}

// Layer is a bounded LRU cache safe for concurrent use.
type Layer struct {
	name string
	opts Options

	mu      sync.Mutex
	order   *list.List
	entries map[string]*list.Element
	bytes   int64

	hits   int64
	misses int64
	evict  int64
}

type entry struct {
	key       string
	value     any
	weight    int64
	version   uint64
	expiresAt time.Time
}

// New creates a named cache layer.
func New(name string, opts Options) *Layer {
	if opts.MaxEntries < 0 {
		opts.MaxEntries = 0
	}

	if opts.Weigh == nil {
		opts.Weigh = func(any) int64 { return 0 }
	}

	return &Layer{
		name:    name,
		opts:    opts,
		order:   list.New(),
		entries: make(map[string]*list.Element),
	}
}

// Name returns the layer name.
func (l *Layer) Name() string {
	return l.name
}

// Get returns a live value for key. A value stored under a different
// generation (version) is treated as absent and invalidated.
func (l *Layer) Get(key string, generation uint64) (any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	element, ok := l.entries[key]
	if !ok {
		l.misses++

		return nil, false
	}

	e := element.Value.(*entry)

	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		l.removeElement(element)
		l.misses++

		return nil, false
	}

	if e.version != generation {
		// Stale generation: drop rather than serve.
		l.removeElement(element)
		l.misses++

		return nil, false
	}

	l.order.MoveToFront(element)
	l.hits++

	return e.value, true
}

// Put stores a value under key with a generation stamp and returns
// the value stored previously, if any.
func (l *Layer) Put(key string, value any, generation uint64) {
	if l.opts.MaxEntries == 0 {
		return // hard-disabled layer
	}

	weight := l.opts.Weigh(value)

	l.mu.Lock()
	defer l.mu.Unlock()

	if element, ok := l.entries[key]; ok {
		old := element.Value.(*entry)

		l.bytes -= old.weight
		old.value = value
		old.weight = weight
		old.version = generation

		if l.opts.TTL > 0 {
			old.expiresAt = time.Now().Add(l.opts.TTL)
		}

		l.bytes += weight
		l.order.MoveToFront(element)

		l.evictOverflow()

		return
	}

	e := &entry{
		key:     key,
		value:   value,
		weight:  weight,
		version: generation,
	}

	if l.opts.TTL > 0 {
		e.expiresAt = time.Now().Add(l.opts.TTL)
	}

	l.entries[key] = l.order.PushFront(e)
	l.bytes += weight

	l.evictOverflow()
}

// Invalidate drops one key.
func (l *Layer) Invalidate(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if element, ok := l.entries[key]; ok {
		l.removeElement(element)
	}
}

// Clear drops every entry.
func (l *Layer) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.opts.OnEvict != nil {
		for element := l.order.Front(); element != nil; element = element.Next() {
			l.opts.OnEvict(element.Value.(*entry).value)
		}
	}

	l.order.Init()
	l.entries = make(map[string]*list.Element)
	l.bytes = 0
}

// Len returns the number of live entries.
func (l *Layer) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.entries)
}

// Stats is a point-in-time layer report.
type Stats struct {
	Name      string  `json:"name"`
	Entries   int     `json:"entries"`
	Bytes     int64   `json:"bytes"`
	Hits      int64   `json:"hits"`
	Misses    int64   `json:"misses"`
	Evictions int64   `json:"evictions"`
	HitRate   float64 `json:"hit_rate"`
}

// Snapshot returns the layer statistics.
func (l *Layer) Snapshot() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()

	s := Stats{
		Name:      l.name,
		Entries:   len(l.entries),
		Bytes:     l.bytes,
		Hits:      l.hits,
		Misses:    l.misses,
		Evictions: l.evict,
	}

	if total := l.hits + l.misses; total > 0 {
		s.HitRate = float64(l.hits) / float64(total)
	}

	return s
}

func (l *Layer) removeElement(element *list.Element) {
	e := element.Value.(*entry)

	l.order.Remove(element)
	delete(l.entries, e.key)
	l.bytes -= e.weight

	if l.opts.OnEvict != nil {
		l.opts.OnEvict(e.value)
	}
}

func (l *Layer) evictOverflow() {
	for {
		if l.opts.MaxEntries > 0 && len(l.entries) > l.opts.MaxEntries {
			l.evictOldest()

			continue
		}

		if l.opts.MaxBytes > 0 && l.bytes > l.opts.MaxBytes {
			if len(l.entries) <= 1 {
				return // keep at least the newest entry
			}

			l.evictOldest()

			continue
		}

		return
	}
}

func (l *Layer) evictOldest() {
	oldest := l.order.Back()
	if oldest == nil {
		return
	}

	l.removeElement(oldest)
	l.evict++
}

// ByteWeight is a convenience weigh function for []byte values.
func ByteWeight(value any) int64 {
	if b, ok := value.([]byte); ok {
		return int64(len(b) + 32)
	}

	return 64
}
