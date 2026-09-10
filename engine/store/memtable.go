package store

// This file implements the memtable layers: the active mutable
// memtable and the immutable frozen tables queued for chunk flush.
//
// Ownership model for values:
//
//   - A memtable value ([]byte) is owned exclusively by the table that
//     stores it. applyUpsert stores a private copy; nothing ever
//     mutates the bytes in place afterwards (a new Upsert REPLACES the
//     slice header with a fresh copy).
//   - Consequently the bytes behind a value are immutable for their
//     entire lifetime. Readers may capture the slice HEADER under a
//     store lock and keep reading the bytes after releasing the lock —
//     this is what makes snapshot iteration allocation-free.
//   - Get returns a defensive copy to callers: the returned buffer is
//     owned by the caller and safe to mutate.
//
// A nil value in a table is a tombstone: the newest state of a key
// that was deleted. Tombstones shadow older levels and the index;
// they are dropped when the table is flushed to a chunk (the delete
// is durable in the WAL until the flush checkpoints past it).

// memtable is one level of pending state.
type memtable struct {
	entries map[[32]byte][]byte // nil value = tombstone
	bytes   int64
}

// newMemtable creates an empty table with a small preallocation.
func newMemtable() *memtable {
	return &memtable{entries: make(map[[32]byte][]byte, 64)}
}

// len returns the number of entries, live or tombstoned.
func (m *memtable) len() int {
	return len(m.entries)
}

// live reports the number of non-tombstone entries.
func (m *memtable) live() int {
	count := 0

	for _, value := range m.entries {
		if value != nil {
			count++
		}
	}

	return count
}

// put stores a private copy of value.
func (m *memtable) put(bin [32]byte, value []byte) {
	if existing, ok := m.entries[bin]; ok && existing != nil {
		m.bytes -= int64(len(existing))
	}

	m.entries[bin] = append([]byte(nil), value...)
	m.bytes += int64(len(value))
}

// tombstone records a delete.
func (m *memtable) tombstone(bin [32]byte) {
	if existing, ok := m.entries[bin]; ok && existing != nil {
		m.bytes -= int64(len(existing))
	}

	m.entries[bin] = nil
}

// remove drops the entry entirely (flush bookkeeping).
func (m *memtable) remove(bin [32]byte) {
	if existing, ok := m.entries[bin]; ok && existing != nil {
		m.bytes -= int64(len(existing))
	}

	delete(m.entries, bin)
}

// frozenTable is an immutable memtable captured for background flush.
type frozenTable struct {
	table *memtable
	// lsn is the WAL watermark at freeze time: every journal record
	// with LSN <= lsn is incorporated in this or an EARLIER frozen
	// table. Advancing the WAL checkpoint beyond lsn is only safe
	// after this table's chunk and metadata are durable.
	lsn uint64
}
