package store

import (
	"container/list"
	"errors"
	"os"
	"sync"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// This file implements the chunk-handle cache: the single owner of open
// chunk file descriptors.
//
// Ownership model (see docs/storage-format.md):
//
//      Open chunk file
//           |
//           v
//      chunkHandleCache (sole owner of the descriptor)
//           |
//           |-- acquire(seq): pin the handle for a reader
//           |-- release(seq): unpin; unpinned entries become evictable
//           |-- evict: LRU eviction of UNPINNED entries closes the file
//           |-- purge(seq): close + forget one entry (compaction victims)
//           `-- closeAll(): close every entry (store shutdown)
//
// Invariants that make Windows deletion safe:
//
//   - A file descriptor is closed BEFORE the store ever removes the
//     underlying file (eviction, purge, closeAll all close first).
//   - Entries with outstanding pins are never evicted or purged; a
//     pinned entry is detached from the LRU list so eviction cannot
//     reach it.
//   - The cache never outlives the store: Store.Close always calls
//     closeAll, so no descriptor owned by the store remains open after
//     Close returns.
//
// os.File.ReadAt is safe for concurrent use (pread without a shared
// file offset), so multiple readers may share one pinned handle.

// errCacheClosed is returned once closeAll has run.
var errCacheClosed = firerrors.New(firerrors.KindFatal,
	Subsystem, "chunk-cache", "handle cache is closed")

// errEntryPinned is returned by purge when a handle is still in use.
var errEntryPinned = firerrors.New(firerrors.KindFatal,
	Subsystem, "chunk-cache", "handle still pinned")

// handleStats are cumulative cache counters for diagnostics.
type handleStats struct {
	Opens     int64
	Closes    int64
	Hits      int64
	Misses    int64
	Evictions int64
}

// chunkHandleCache is a bounded LRU of open chunk files with
// reference-counted pins and deterministic close semantics.
//
// pathFor resolves a chunk sequence number to its file path and is
// invoked ONLY on cache misses, so hot reads pay no path formatting
// cost.
type chunkHandleCache struct {
	mu      sync.Mutex
	maxOpen int
	pathFor func(seq uint32) string
	entries map[uint32]*handleEntry
	lru     *list.List // back = least recently used; pinned entries absent
	closed  bool

	stats handleStats
}

// handleEntry is one cached open file.
type handleEntry struct {
	seq  uint32
	file *os.File
	refs int
	elem *list.Element // nil while pinned (refs > 0)
}

// newChunkHandleCache creates a cache holding at most maxOpen files.
func newChunkHandleCache(maxOpen int, pathFor func(seq uint32) string) *chunkHandleCache {
	if maxOpen < 1 {
		maxOpen = 1
	}

	return &chunkHandleCache{
		maxOpen: maxOpen,
		pathFor: pathFor,
		entries: make(map[uint32]*handleEntry, maxOpen),
		lru:     list.New(),
	}
}

// acquire pins the handle for seq, opening the file if needed. The
// caller MUST call release(seq) exactly once for each successful
// acquire.
func (c *chunkHandleCache) acquire(seq uint32) (*os.File, error) {
	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()

		return nil, errCacheClosed
	}

	if entry, ok := c.entries[seq]; ok {
		entry.refs++
		if entry.elem != nil {
			c.lru.Remove(entry.elem)
			entry.elem = nil // pinned: not evictable
		}

		c.stats.Hits++
		c.mu.Unlock()

		return entry.file, nil
	}

	// Make room for a new descriptor. Pinned entries are not in the
	// LRU, so only genuinely idle handles are closed here.
	for len(c.entries) >= c.maxOpen {
		if !c.evictOldestLocked() {
			break
		}
	}

	c.mu.Unlock()

	file, err := os.Open(c.pathFor(seq))
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "read", "open chunk %d", seq)
	}

	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()
		_ = file.Close()

		return nil, errCacheClosed
	}

	// A concurrent second opener may have inserted an entry while the
	// file was being opened; the first insertion wins and the later
	// descriptor is closed immediately.
	if existing, ok := c.entries[seq]; ok {
		_ = file.Close()
		existing.refs++

		if existing.elem != nil {
			c.lru.Remove(existing.elem)
			existing.elem = nil
		}

		c.mu.Unlock()

		return existing.file, nil
	}

	c.entries[seq] = &handleEntry{seq: seq, file: file, refs: 1}
	c.stats.Opens++
	c.stats.Misses++
	c.mu.Unlock()

	return file, nil
}

// release drops one pin. When the last pin is dropped the entry merely
// becomes evictable; it stays open until eviction, purge or closeAll.
func (c *chunkHandleCache) release(seq uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[seq]
	if !ok {
		return
	}

	if entry.refs > 0 {
		entry.refs--
	}

	if entry.refs == 0 && entry.elem == nil {
		entry.elem = c.lru.PushFront(entry)
		c.evictOverflowLocked()
	}
}

// purge closes and forgets the entry for seq. It fails if the handle
// is still pinned; the caller must guarantee no readers remain (the
// store enforces this with its operation and scan barriers) before
// purging.
func (c *chunkHandleCache) purge(seq uint32) error {
	c.mu.Lock()

	entry, ok := c.entries[seq]
	if !ok {
		c.mu.Unlock()

		return nil
	}

	if entry.refs > 0 {
		c.mu.Unlock()

		return errEntryPinned
	}

	c.removeEntryLocked(entry)
	c.stats.Closes++
	c.mu.Unlock()

	if err := entry.file.Close(); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "compact", "close chunk %d", seq)
	}

	return nil
}

// closeAll closes every cached descriptor and disables the cache. It
// is idempotent and is called by Store.Close after all operations have
// drained, so no reader can be mid-read at this point.
func (c *chunkHandleCache) closeAll() error {
	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()

		return nil
	}

	c.closed = true

	entries := make([]*handleEntry, 0, len(c.entries))
	for _, entry := range c.entries {
		entries = append(entries, entry)
	}

	c.entries = make(map[uint32]*handleEntry)
	c.lru.Init()
	c.mu.Unlock()

	// Close outside the lock: the cache is drained and disabled, so
	// there is no contention left; closing may syscall-block.
	var errs []error

	for _, entry := range entries {
		c.stats.Closes++

		if err := entry.file.Close(); err != nil {
			errs = append(errs, firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "close", "close chunk handle"))
		}
	}

	return errors.Join(errs...)
}

// len reports the number of cached descriptors (diagnostics).
func (c *chunkHandleCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.entries)
}

// snapshotStats copies the cumulative counters (diagnostics).
func (c *chunkHandleCache) snapshotStats() handleStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.stats
}

// evictOldestLocked closes the least recently used unpinned entry. It
// reports whether an entry was evicted. The close syscall runs under
// the lock: closing a read-only handle is a fast operation and the
// lock discipline stays trivially correct.
func (c *chunkHandleCache) evictOldestLocked() bool {
	oldest := c.lru.Back()
	if oldest == nil {
		return false
	}

	entry := oldest.Value.(*handleEntry)

	c.removeEntryLocked(entry)
	c.stats.Evictions++
	c.stats.Closes++

	if err := entry.file.Close(); err != nil {
		// The descriptor is already forgotten; a read-only handle close
		// failure cannot affect correctness. Keep the cache consistent
		// and continue.
		_ = err
	}

	return true
}

// evictOverflowLocked enforces the maxOpen bound after inserting an
// unpinned entry.
func (c *chunkHandleCache) evictOverflowLocked() {
	for len(c.entries) > c.maxOpen && c.lru.Len() > 0 {
		if !c.evictOldestLocked() {
			return
		}
	}
}

// removeEntryLocked unlinks entry from the map and LRU list.
func (c *chunkHandleCache) removeEntryLocked(entry *handleEntry) {
	if entry.elem != nil {
		c.lru.Remove(entry.elem)
		entry.elem = nil
	}

	delete(c.entries, entry.seq)
}
