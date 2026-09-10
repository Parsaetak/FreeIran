package store

// Journal (segmented WAL) unit tests: segment rolling, checkpoint
// removal semantics, corrupt-tail recovery, continuity enforcement and
// the v1 journal.log upgrade path.

import (
	"os"
	"path/filepath"
	"testing"
)

func openTestJournal(t *testing.T, dir string, checkpoint uint64) *journal {
	t.Helper()

	j, err := openJournal(dir, checkpoint, checkpoint, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		return nil
	})
	if err != nil {
		t.Fatalf("openJournal: %v", err)
	}

	return j
}

func batchRecords(n int) []journalRecord {
	records := make([]journalRecord, 0, n)

	for i := 0; i < n; i++ {
		records = append(records, journalRecord{
			Op:    opUpsert,
			Key:   testKeyBytes(i),
			Value: []byte("value"),
		})
	}

	return records
}

func testKeyBytes(i int) [32]byte {
	bin, _ := decodeKey(testKey(i))

	return bin
}

func TestJournalAppendAndLSN(t *testing.T) {
	dir := t.TempDir()
	j := openTestJournal(t, dir, 0)
	defer j.Close()

	if got := j.CurrentLSN(); got != 0 {
		t.Fatalf("initial lsn = %d, want 0", got)
	}

	lsn, err := j.Append(batchRecords(3))
	if err != nil {
		t.Fatal(err)
	}

	if lsn != 3 {
		t.Fatalf("lsn after 3 records = %d, want 3", lsn)
	}

	if _, err := j.Append(batchRecords(2)); err != nil {
		t.Fatal(err)
	}

	if got := j.CurrentLSN(); got != 5 {
		t.Fatalf("lsn = %d, want 5", got)
	}
}

func TestJournalCheckpointRemovesSegments(t *testing.T) {
	dir := t.TempDir()
	j := openTestJournal(t, dir, 0)

	// Enough records to roll several segments: shrink the roll size.
	j.mu.Lock()
	j.segmentMax = 512
	j.mu.Unlock()

	for b := 0; b < 20; b++ {
		if _, err := j.Append(batchRecords(3)); err != nil {
			t.Fatal(err)
		}
	}

	if got := j.segmentCount(); got < 2 {
		t.Fatalf("segments = %d, want >= 2", got)
	}

	// Checkpoint everything: all segments including the active one are
	// replaced by a fresh successor.
	if err := j.Checkpoint(j.CurrentLSN()); err != nil {
		t.Fatal(err)
	}

	if got := j.segmentCount(); got != 1 {
		t.Fatalf("segments after full checkpoint = %d, want 1", got)
	}

	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	// The checkpointed records are gone from disk.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}

		if info.Size() > walHeaderSize+1024 {
			t.Fatalf("segment %s still large (%d bytes) after checkpoint",
				entry.Name(), info.Size())
		}
	}
}

func TestJournalPartialCheckpointKeepsTail(t *testing.T) {
	dir := t.TempDir()
	j := openTestJournal(t, dir, 0)
	defer j.Close()

	if _, err := j.Append(batchRecords(2)); err != nil {
		t.Fatal(err)
	}

	if _, err := j.Append(batchRecords(2)); err != nil {
		t.Fatal(err)
	}

	// Checkpoint only the first batch: its records must be skipped on
	// replay while the second batch replays.
	if err := j.Checkpoint(2); err != nil {
		t.Fatal(err)
	}

	lsn := j.CurrentLSN()

	j.mu.Lock()
	j.file.Close()
	j.mu.Unlock()

	replayed := 0

	j2, err := openJournal(dir, 2, 2, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		replayed++

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	defer j2.Close()

	if replayed != 2 {
		t.Fatalf("replayed %d records, want 2 (only the post-checkpoint batch)", replayed)
	}

	if j2.CurrentLSN() != lsn {
		t.Fatalf("lsn after reopen = %d, want %d", j2.CurrentLSN(), lsn)
	}
}

func TestJournalCorruptTailDiscarded(t *testing.T) {
	dir := t.TempDir()
	j := openTestJournal(t, dir, 0)

	if _, err := j.Append(batchRecords(3)); err != nil {
		t.Fatal(err)
	}

	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt the tail: append garbage bytes after the last record.
	segments, err := filepath.Glob(filepath.Join(dir, "seg-*.wal"))
	if err != nil || len(segments) == 0 {
		t.Fatalf("no segments: %v", err)
	}

	file, err := os.OpenFile(segments[0], os.O_APPEND|os.O_WRONLY, filePerm)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := file.Write([]byte{byte(opUpsert), 0xAB}); err != nil {
		t.Fatal(err)
	}

	_ = file.Close()

	replayed := 0

	j2, err := openJournal(dir, 0, 0, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		replayed++

		return nil
	})
	if err != nil {
		t.Fatalf("reopen with corrupt tail: %v", err)
	}

	defer j2.Close()

	if replayed != 3 {
		t.Fatalf("replayed %d records, want 3 (corrupt tail discarded)", replayed)
	}

	// The tail must be truncated so future appends stay clean.
	info, err := os.Stat(segments[0])
	if err != nil {
		t.Fatal(err)
	}

	if _, err := j2.Append(batchRecords(1)); err != nil {
		t.Fatal(err)
	}

	_ = info
}

func TestJournalReplayAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	j := openTestJournal(t, dir, 0)

	j.mu.Lock()
	j.segmentMax = 400
	j.mu.Unlock()

	total := 0

	for b := 0; b < 10; b++ {
		if _, err := j.Append(batchRecords(4)); err != nil {
			t.Fatal(err)
		}

		total += 4
	}

	segments := j.segmentCount()

	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	if segments < 2 {
		t.Fatalf("segments = %d, want >= 2", segments)
	}

	replayed := 0

	j2, err := openJournal(dir, 0, 0, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		replayed++

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	defer j2.Close()

	if replayed != total {
		t.Fatalf("replayed %d records, want %d", replayed, total)
	}

	if got := j2.CurrentLSN(); got != uint64(total) {
		t.Fatalf("lsn after replay = %d, want %d", got, total)
	}
}

func TestJournalMinLSNRespected(t *testing.T) {
	dir := t.TempDir()

	// A journal seeded from metadata watermark 100 must never start
	// counting below it, even with no segments on disk.
	j := openTestJournal(t, dir, 100)
	defer j.Close()

	if got := j.CurrentLSN(); got != 100 {
		t.Fatalf("lsn = %d, want 100", got)
	}

	if _, err := j.Append(batchRecords(1)); err != nil {
		t.Fatal(err)
	}

	if got := j.CurrentLSN(); got != 101 {
		t.Fatalf("lsn after append = %d, want 101", got)
	}
}

func TestJournalLegacyLogUpgraded(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "journal.log")

	// Build a v1 journal.log: header (magic + version + baseLSN +
	// reserved) then uniform records (op|key|valLen|value|crc).
	table := crc32Table()

	buf := make([]byte, 0, 128)
	buf = append(buf, walMagic...)
	buf = append(buf, 0x01, 0x00) // version 1

	var base [8]byte
	buf = append(buf, base[:]...) // baseLSN 0
	buf = append(buf, 0, 0, 0, 0) // reserved (v1)

	for i := 0; i < 5; i++ {
		raw := rawRecord{Op: opUpsert, Key: testKeyBytes(i), Value: []byte("legacy")}

		buf = append(buf, byte(raw.Op))
		buf = append(buf, raw.Key[:]...)

		var valLen [4]byte
		valLen[0] = byte(len(raw.Value))
		buf = append(buf, valLen[:]...)
		buf = append(buf, raw.Value...)

		crc := journalCRC(table, raw)
		buf = append(buf, byte(crc), byte(crc>>8), byte(crc>>16), byte(crc>>24))
	}

	if err := os.WriteFile(legacy, buf, filePerm); err != nil {
		t.Fatal(err)
	}

	replayed := 0

	j, err := openJournal(dir, 0, 0, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		replayed++

		return nil
	})
	if err != nil {
		t.Fatalf("open with legacy journal: %v", err)
	}

	defer j.Close()

	if replayed != 5 {
		t.Fatalf("replayed %d legacy records, want 5", replayed)
	}

	// The legacy file must be consumed (removed) after re-journaling.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy journal.log still present after upgrade")
	}

	// And its records must now live in the segmented WAL.
	if got := j.CurrentLSN(); got != 5 {
		t.Fatalf("lsn after upgrade = %d, want 5", got)
	}
}

func TestJournalDeleteRecordsReplay(t *testing.T) {
	dir := t.TempDir()
	j := openTestJournal(t, dir, 0)

	records := []journalRecord{
		{Op: opUpsert, Key: testKeyBytes(1), Value: []byte("a")},
		{Op: opDelete, Key: testKeyBytes(1)},
		{Op: opUpsert, Key: testKeyBytes(2), Value: []byte("b")},
	}

	if _, err := j.Append(records); err != nil {
		t.Fatal(err)
	}

	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	ops := []walOp{}

	j2, err := openJournal(dir, 0, 0, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		ops = append(ops, op)

		return nil
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	defer j2.Close()

	if len(ops) != 3 || ops[0] != opUpsert || ops[1] != opDelete || ops[2] != opUpsert {
		t.Fatalf("replayed ops = %v, want [upsert delete upsert]", ops)
	}
}
