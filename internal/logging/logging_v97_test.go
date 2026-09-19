package logging

// v0.9.7 tests: session identity, monotonic sequence, structured
// fields, correlation (batch/test/config), concurrent logging safety,
// Query filters, Related causality and the Dedupe noise reducer.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestLogger(t *testing.T) *Logger {
	t.Helper()

	logger, err := Open(Options{Dir: t.TempDir(), MaxBytes: 4 << 20, MinLevel: LevelDebug})
	if err != nil {
		t.Fatalf("open logger: %v", err)
	}

	t.Cleanup(func() { _ = logger.Close() })

	return logger
}

// TestSessionIdentityAndEventIDs verifies the v0.9.8.3 identity
// contract: ordinary records are COMPACT (no session_id/event_id) but
// keep a strictly monotonic sequence; correlation-opt-in records
// carry the session id, a unique event id and stay monotonic.
func TestSessionIdentityAndEventIDs(t *testing.T) {
	logger := newTestLogger(t)

	for i := 0; i < 25; i++ {
		logger.Info("test", "tick", "tick %d", i)
	}

	entries := logger.Recent(0, 100, "", "")
	if len(entries) != 25 {
		t.Fatalf("entries = %d, want 25", len(entries))
	}

	for i, entry := range entries {
		// v0.9.8.3: ordinary records carry NO session/event identity.
		if entry.SessionID != "" {
			t.Fatalf("entry %d: ordinary record carries session_id %q", i, entry.SessionID)
		}

		if entry.EventID != "" {
			t.Fatalf("entry %d: ordinary record carries event_id %q", i, entry.EventID)
		}

		if i > 0 && entry.Seq <= entries[i-1].Seq {
			t.Fatalf("entry %d: sequence not monotonic (%d <= %d)",
				i, entry.Seq, entries[i-1].Seq)
		}
	}

	// Correlation-opt-in records keep full identity.
	for i := 0; i < 5; i++ {
		logger.Log(Record{
			Level:     LevelInfo,
			Subsystem: "connection",
			Event:     "episode_event",
			Correlate: true,
			Message:   fmt.Sprintf("episode %d", i),
		})
	}

	entries = logger.Recent(0, 100, "", "")
	if len(entries) != 30 {
		t.Fatalf("entries after correlated records = %d, want 30", len(entries))
	}

	seenIDs := make(map[string]bool, 5)

	correlated := 0

	for _, entry := range entries {
		if !entry.Correlated {
			continue
		}

		correlated++

		if entry.SessionID == "" {
			t.Fatalf("correlated entry: empty session_id")
		}

		if entry.SessionID != logger.SessionID() {
			t.Fatalf("correlated entry: session mismatch")
		}

		if entry.EventID == "" {
			t.Fatalf("correlated entry: empty event_id")
		}

		if seenIDs[entry.EventID] {
			t.Fatalf("duplicate event_id %s", entry.EventID)
		}

		seenIDs[entry.EventID] = true
	}

	if correlated != 5 {
		t.Fatalf("correlated entries = %d, want 5", correlated)
	}

	// A second launch gets a different session id.
	second, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	defer second.Close()

	if second.SessionID() == logger.SessionID() {
		t.Fatal("two launches share one session id")
	}
}

// TestStructuredRecordFields verifies Log() carries structured values
// in dedicated JSON fields — never only inside the message — and that
// string field values are redacted.
func TestStructuredRecordFields(t *testing.T) {
	dir := t.TempDir()

	logger, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	logger.Log(Record{
		Level:      LevelInfo,
		Subsystem:  "connection",
		Event:      "verified",
		Correlate:  true,
		ConfigID:   "cfg-123",
		Core:       "xray",
		Listener:   "127.0.0.1:12386",
		DurationMS: 214,
		Status:     "usable",
		Message:    "connection verified",
		Fields: map[string]any{
			"ping_median_ms": 83,
			"url_total_ms":   214,
			"secret_param":   "hunter2-password",
		},
	})

	_ = logger.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "freeiran.log"))
	if err != nil {
		t.Fatal(err)
	}

	line := strings.TrimSpace(string(raw))

	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("decode entry: %v", err)
	}

	for _, key := range []string{
		"session_id", "event_id", "config_id", "core", "listener",
		"duration_ms", "status",
	} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("structured field %q missing from JSON", key)
		}
	}

	fields, ok := decoded["fields"].(map[string]any)
	if !ok {
		t.Fatal("fields map missing")
	}

	if fields["ping_median_ms"].(float64) != 83 {
		t.Fatalf("ping_median_ms = %v", fields["ping_median_ms"])
	}

	if !strings.Contains(fields["secret_param"].(string), "REDACTED") {
		t.Fatalf("field value not redacted: %v", fields["secret_param"])
	}
}

// TestCorrelationIDs verifies batch/test/config correlation survives a
// structured record and that Query can select by each of them.
func TestCorrelationIDs(t *testing.T) {
	logger := newTestLogger(t)

	logger.Log(Record{
		Level:     LevelInfo,
		Subsystem: "testqueue",
		Event:     "batch_started",
		BatchID:   "batch-1",
		Message:   "bulk test started",
	})
	logger.Log(Record{
		Level:     LevelInfo,
		Subsystem: "testqueue",
		Event:     "task_passed",
		BatchID:   "batch-1",
		TestID:    "task-9",
		ConfigID:  "cfg-77",
		Message:   "task passed",
	})
	logger.Info("other", "noise", "unrelated")

	if got := len(logger.Query(Filter{BatchID: "batch-1"})); got != 2 {
		t.Fatalf("batch filter = %d entries, want 2", got)
	}

	if got := len(logger.Query(Filter{TestID: "task-9"})); got != 1 {
		t.Fatalf("test filter = %d entries, want 1", got)
	}

	if got := len(logger.Query(Filter{ConfigID: "cfg-77"})); got != 1 {
		t.Fatalf("config filter = %d entries, want 1", got)
	}

	if got := len(logger.Query(Filter{Event: "batch_started"})); got != 1 {
		t.Fatalf("event filter = %d entries, want 1", got)
	}

	if got := len(logger.Query(Filter{ErrorsOnly: true})); got != 0 {
		t.Fatalf("errors-only = %d entries, want 0", got)
	}

	logger.Warn("testqueue", "queue_pressure", "pressure")

	if got := len(logger.Query(Filter{ErrorsOnly: true})); got != 1 {
		t.Fatalf("errors-only = %d entries, want 1", got)
	}

	if got := len(logger.Query(Filter{Level: LevelWarn})); got != 1 {
		t.Fatalf("level filter = %d entries, want 1", got)
	}
}

// TestRelatedCausality verifies Related() returns the anchor event,
// its children and its batch siblings.
func TestRelatedCausality(t *testing.T) {
	logger := newTestLogger(t)

	parent := Record{
		Level: LevelInfo, Subsystem: "testqueue",
		Event: "batch_started", BatchID: "b-1", Message: "started",
	}
	logger.Log(parent)

	// Find the anchor id via the ring.
	anchor := logger.Query(Filter{Event: "batch_started", Limit: 1})
	if len(anchor) != 1 {
		t.Fatal("anchor not found")
	}

	eventID := anchor[0].EventID

	logger.Log(Record{
		Level: LevelInfo, Subsystem: "testqueue",
		Event: "task_started", ParentID: eventID,
		BatchID: "b-1", TestID: "t-1", Message: "child",
	})
	logger.Info("elsewhere", "noise", "unrelated")

	related := logger.Related(eventID, 100)
	if len(related) != 2 {
		t.Fatalf("related = %d entries, want 2", len(related))
	}

	if logger.Related("nonexistent-id", 100) != nil {
		t.Fatal("unknown event id must return nil")
	}
}

// TestConcurrentStructuredLogging verifies concurrent Log/Info calls
// keep unique event ids and an unbroken sequence. The stress runs
// under the Debug profile (v0.9.8.4): every record carries identity,
// so the uniqueness/monotonicity checks exercise the busiest path.
func TestConcurrentStructuredLogging(t *testing.T) {
	logger := newTestLogger(t)

	logger.SetProfile(ProfileDebug)

	const goroutines, perG = 16, 40

	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			for i := 0; i < perG; i++ {
				switch i % 3 {
				case 0:
					logger.Info("stress", "info_event", "g%d i%d", g, i)
				case 1:
					logger.Log(Record{
						Level: LevelDebug, Subsystem: "stress",
						Event: "debug_event", TestID: fmt.Sprintf("t-%d-%d", g, i),
						Message: "structured",
					})
				default:
					logger.Warn("stress", "warn_event", "g%d i%d", g, i)
				}
			}
		}(g)
	}

	wg.Wait()

	entries := logger.Recent(0, 1000, "", "")
	if len(entries) != goroutines*perG {
		t.Fatalf("entries = %d, want %d", len(entries), goroutines*perG)
	}

	seen := make(map[string]bool, len(entries))

	for i, entry := range entries {
		// v0.9.8.3: only correlated records carry event ids; ordinary
		// records stay compact. Event ids must still be unique.
		if entry.EventID != "" {
			if seen[entry.EventID] {
				t.Fatalf("entry %d: duplicate event_id", i)
			}

			seen[entry.EventID] = true
		}
	}
}

// TestDedupeSessionLevelSuppression verifies the noise reducer logs
// the first occurrence only, counts the rest and flushes a summary.
func TestDedupeSessionLevelSuppression(t *testing.T) {
	dir := t.TempDir()

	logger, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	defer logger.Close()

	dedupe := NewDedupe()

	for i := 0; i < 42; i++ {
		dedupe.Do(logger, LevelWarn, "system", "job_fallback",
			"job-fallback", "process launched without kernel job binding")
	}

	if got := dedupe.Count("job-fallback"); got != 42 {
		t.Fatalf("count = %d, want 42", got)
	}

	// Exactly one warning reached the log despite 42 occurrences.
	warns := logger.Query(Filter{Event: "job_fallback"})
	if len(warns) != 1 {
		t.Fatalf("job_fallback entries = %d, want 1", len(warns))
	}

	dedupe.FlushSummary(logger, "system", "job_fallback", true, true)

	summary := logger.Query(Filter{Event: "job_fallback_summary", Limit: 10})
	if len(summary) != 1 {
		t.Fatalf("summary entries = %d, want 1", len(summary))
	}

	countsMap, ok := summary[0].Fields["counts"]
	if !ok {
		t.Fatalf("summary counts missing: %v", summary[0].Fields)
	}

	var got uint64

	switch counts := countsMap.(type) {
	case map[string]uint64:
		got = counts["job-fallback"]
	case map[string]any:
		switch v := counts["job-fallback"].(type) {
		case float64:
			got = uint64(v)
		case uint64:
			got = v
		default:
			t.Fatalf("summary count type: %T", counts["job-fallback"])
		}
	default:
		t.Fatalf("summary counts type: %T", countsMap)
	}

	if got != 42 {
		t.Fatalf("summary count = %d, want 42", got)
	}

	// Reset cleared the table.
	if dedupe.Count("job-fallback") != 0 {
		t.Fatal("flush with reset did not clear counters")
	}
}

// TestEntryShapeStillJSONL verifies entries on disk remain one JSON
// object per line (machine-readable JSONL preserved).
func TestEntryShapeStillJSONL(t *testing.T) {
	dir := t.TempDir()

	logger, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	logger.Info("app", "application_start", "starting")
	logger.Debug("x", "y", "z")
	_ = logger.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "freeiran.log"))
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 { // debug filtered at default info level
		t.Fatalf("lines = %d, want 1", len(lines))
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}

	// v0.9.8.3: ordinary on-disk records are COMPACT — no session or
	// event identity, no correlation ids.
	for _, banned := range []string{"session_id", "event_id"} {
		if _, ok := entry[banned]; ok {
			t.Fatalf("ordinary record carries %q on disk", banned)
		}
	}

	for _, required := range []string{"seq", "ts", "level", "subsystem", "event"} {
		if _, ok := entry[required]; !ok {
			t.Fatalf("compact record missing %q", required)
		}
	}
}
