package logging

// Tests for the persistent runtime log (§49 Logging requirements):
// file creation, size rotation with bounded backups, startup
// recovery, redaction of credential material, concurrent writes and
// shutdown flush.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openTestLogger opens a logger with tight rotation limits.
func openTestLogger(t *testing.T, dir string, maxBytes int64) *Logger {
	t.Helper()

	logger, err := Open(Options{
		Dir:        dir,
		MaxBytes:   maxBytes,
		MaxBackups: 2,
	})
	if err != nil {
		t.Fatalf("open logger: %v", err)
	}

	t.Cleanup(func() {
		_ = logger.Close()
	})

	return logger
}

// readLines decodes every JSON line of a log file.
func readLines(t *testing.T, path string) []Entry {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		t.Fatal(err)
	}

	defer file.Close()

	var entries []Entry

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Bytes()

		if len(line) == 0 {
			continue
		}

		var entry Entry

		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}

		entries = append(entries, entry)
	}

	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	return entries
}

// TestLogFileCreatedAndStructured verifies the file exists, every
// line is JSON with the full §17 field shape, and shutdown flushes.
func TestLogFileCreatedAndStructured(t *testing.T) {
	dir := t.TempDir()

	logger := openTestLogger(t, dir, 1<<20)

	logger.Info("app", "application_start", "starting %s", "v0.5.0")
	logger.Warn("store", "store_error", "slow flush")
	logger.Error("core", "core_error", "start", "process", "V2Ray failed to start: exit code %d", 1)

	if err := logger.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entries := readLines(t, filepath.Join(dir, "freeiran.log"))

	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}

	first := entries[0]

	if first.Level != LevelInfo || first.Subsystem != "app" ||
		first.Event != "application_start" || first.Message != "starting v0.5.0" {
		t.Fatalf("entry = %+v", first)
	}

	if _, err := time.Parse(time.RFC3339Nano, first.Time); err != nil {
		t.Fatalf("timestamp not RFC3339: %v", err)
	}

	if first.Seq == 0 {
		t.Fatal("sequence numbers must start at 1")
	}

	if entries[2].Operation != "start" || entries[2].ErrorKind != "process" {
		t.Fatalf("error fields missing: %+v", entries[2])
	}

	// After Close the file is complete: reopening for append works
	// and the next session continues the sequence.
	reopened, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	reopened.Info("app", "application_ready", "second session")

	entries = readLines(t, filepath.Join(dir, "freeiran.log"))

	if len(entries) != 4 {
		t.Fatalf("after reopen entries = %d, want 4", len(entries))
	}

	_ = reopened.Close()
}

// TestLogRotationBoundsBackups verifies size rotation keeps at most
// MaxBackups numbered files and the primary stays under the limit.
func TestLogRotationBoundsBackups(t *testing.T) {
	dir := t.TempDir()

	const maxBytes = 512

	logger := openTestLogger(t, dir, maxBytes)

	for i := 0; i < 200; i++ {
		logger.Info("test", "rotation_fill", "%s", strings.Repeat("x", 120))
	}

	_ = logger.Close()

	// Enumerate the log files.
	matches, err := filepath.Glob(filepath.Join(dir, "freeiran.log*"))
	if err != nil {
		t.Fatal(err)
	}

	// Primary + at most MaxBackups backups.
	if len(matches) > 1+2 {
		t.Fatalf("log files = %v, want at most 3", matches)
	}

	primary := filepath.Join(dir, "freeiran.log")

	info, err := os.Stat(primary)
	if err != nil {
		t.Fatal(err)
	}

	if info.Size() > maxBytes+256 {
		t.Fatalf("primary log = %d bytes, must stay bounded", info.Size())
	}

	// Every produced file parses fully as JSON lines.
	for _, path := range matches {
		readLines(t, path)
	}
}

// TestLogRotationRecovery verifies an oversized primary file left by
// an interrupted session is rotated on the next Open.
func TestLogRotationRecovery(t *testing.T) {
	dir := t.TempDir()

	primary := filepath.Join(dir, "freeiran.log")

	// Simulate an interrupted session: a huge primary file.
	if err := os.WriteFile(primary,
		[]byte(strings.Repeat("y", 4096)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logger, err := Open(Options{Dir: dir, MaxBytes: 1024, MaxBackups: 2})
	if err != nil {
		t.Fatalf("open after interrupted rotation: %v", err)
	}

	logger.Info("app", "application_start", "recovery session")

	if _, err := os.Stat(primary + ".1"); err != nil {
		t.Fatalf("oversized primary was not rotated on open: %v", err)
	}

	_ = logger.Close()

	entries := readLines(t, primary)

	if len(entries) != 1 || entries[0].Event != "application_start" {
		t.Fatalf("recovery session entries = %+v", entries)
	}
}

// TestLogRedaction verifies credential material never reaches the
// stored entries (§19).
func TestLogRedaction(t *testing.T) {
	dir := t.TempDir()

	logger := openTestLogger(t, dir, 1<<20)

	logger.Info("core", "core_start", "launching vless://secret-uuid@host:443?security=tls")
	logger.Info("connection", "connection_start", "config id 11111111-1111-1111-1111-111111111111")
	logger.Info("parser", "parse", "password=hunter2 token=abc123 session=ok")
	logger.Info("store", "store_open", "url https://ex.com/?token=tok123&page=2")

	entries := logger.Recent(0, 100, "", "")

	if len(entries) != 4 {
		t.Fatalf("entries = %d", len(entries))
	}

	for _, entry := range entries {
		for _, leak := range []string{
			"secret-uuid", "11111111-1111", "hunter2", "abc123", "tok123",
		} {
			if strings.Contains(entry.Message, leak) {
				t.Fatalf("entry %s leaks %q: %s", entry.Event, leak, entry.Message)
			}
		}
	}

	// The redacted forms keep diagnostics useful.
	if !strings.HasPrefix(entries[0].Message, "launching vless://[REDACTED]") {
		t.Fatalf("protocol URL redaction wrong: %s", entries[0].Message)
	}

	if !strings.Contains(entries[2].Message, "session=ok") {
		t.Fatalf("benign fields must survive: %s", entries[2].Message)
	}

	// Explicit secrets.
	got := RedactWithSecrets("connect to super-secret-value now", []string{"super-secret-value"})
	if strings.Contains(got, "super-secret-value") {
		t.Fatalf("explicit secret survived: %s", got)
	}

	// The file content must be equally clean.
	raw, err := os.ReadFile(filepath.Join(dir, "freeiran.log"))
	if err != nil {
		t.Fatal(err)
	}

	for _, leak := range []string{"secret-uuid", "hunter2", "abc123"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("log file leaks %q", leak)
		}
	}
}

// TestLogConcurrentWrites verifies concurrent subsystems never
// interleave partial lines and every entry survives.
func TestLogConcurrentWrites(t *testing.T) {
	dir := t.TempDir()

	logger := openTestLogger(t, dir, 8<<20)

	const writers, perWriter = 8, 100

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			for i := 0; i < perWriter; i++ {
				logger.Info("load", "concurrent_write", "writer %d item %d", w, i)
			}
		}(w)
	}

	wg.Wait()

	_ = logger.Close()

	entries := readLines(t, filepath.Join(dir, "freeiran.log"))

	if len(entries) != writers*perWriter {
		t.Fatalf("entries = %d, want %d", len(entries), writers*perWriter)
	}

	seen := make(map[uint64]bool, len(entries))

	for _, entry := range entries {
		if seen[entry.Seq] {
			t.Fatalf("duplicate sequence %d", entry.Seq)
		}

		seen[entry.Seq] = true
	}
}

// TestLogRecentIncrementalAndFilter verifies the incremental UI read:
// sequence-based paging, subsystem filter and query filter.
func TestLogRecentIncrementalAndFilter(t *testing.T) {
	dir := t.TempDir()

	logger := openTestLogger(t, dir, 1<<20)

	for i := 0; i < 30; i++ {
		logger.Info("engine", "tick", "tick %d", i)
		logger.Info("ui", "hover", "hover %d", i)
	}

	all := logger.Recent(0, 1000, "", "")
	if len(all) != 60 {
		t.Fatalf("all = %d, want 60", len(all))
	}

	engine := logger.Recent(0, 1000, "engine", "")
	if len(engine) != 30 {
		t.Fatalf("engine = %d, want 30", len(engine))
	}

	page := logger.Recent(0, 10, "", "")
	if len(page) != 10 {
		t.Fatalf("page = %d, want 10", len(page))
	}

	since := page[len(page)-1].Seq

	next := logger.Recent(since, 10, "", "")
	if len(next) == 0 {
		t.Fatal("incremental page must continue after sinceSeq")
	}

	if next[0].Seq <= since {
		t.Fatal("incremental results must be newer than sinceSeq")
	}

	match := logger.Recent(0, 10, "", "tick 2")
	if len(match) == 0 {
		t.Fatal("query filter must match")
	}

	logger.Clear()

	if got := logger.Recent(0, 10, "", ""); len(got) != 0 {
		t.Fatalf("clear left %d entries", len(got))
	}
}

// TestLogShutdownFlush verifies entries written immediately before
// Close are durable on disk (shutdown ordering guarantee).
func TestLogShutdownFlush(t *testing.T) {
	dir := t.TempDir()

	logger, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	logger.Info("app", "shutdown_start", "stopping subsystems")
	logger.Info("app", "shutdown_complete", "bye")

	if err := logger.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entries := readLines(t, filepath.Join(dir, "freeiran.log"))

	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (flush on shutdown)", len(entries))
	}

	// Double close is safe.
	if err := logger.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestLogSubscribeLive verifies subscribers receive entries live and
// slow subscribers never block the engine.
func TestLogSubscribeLive(t *testing.T) {
	dir := t.TempDir()

	logger := openTestLogger(t, dir, 1<<20)

	ch, cancel := logger.Subscribe(4)

	logger.Info("test", "sub_a", "first")
	received := <-ch

	if received.Event != "sub_a" {
		t.Fatalf("received %+v", received)
	}

	// Flood past the buffer: drops, never blocks.
	for i := 0; i < 100; i++ {
		logger.Info("test", "flood", "message %d", i)
	}

	cancel()

	// Drain entries buffered before the cancel. After cancel returns
	// no new writes reach the channel, so draining is deterministic.
drain:
	for {
		select {
		case <-ch:
			continue drain
		default:
			break drain
		}
	}

	logger.Info("test", "sub_b", "after cancel")

	select {
	case entry := <-ch:
		t.Fatalf("entry after cancel: %+v", entry)
	default:
	}
}

// TestNoGlobalIsNoOp verifies the engine-level helpers are safe
// no-ops without an installed logger (library/test silence).
func TestNoGlobalIsNoOp(t *testing.T) {
	previous := global.Load()

	global.Store(nil)

	defer global.Store(previous)

	E("any", "event", "must not panic")
	W("any", "event", "must not panic")
	Err("any", "event", "op", "kind", "must not panic")
	D("any", "event", "must not panic")
}

// TestGlobalIntegration verifies the global helpers route into the
// installed logger.
func TestGlobalIntegration(t *testing.T) {
	dir := t.TempDir()

	logger := openTestLogger(t, dir, 1<<20)

	SetGlobal(logger)
	defer SetGlobal(nil)

	E("store", "store_open", "opened at %s", "/tmp/data")
	Err("migrate", "migration_error", "rename", "environment", "preserve legacy file failed")

	entries := logger.Recent(0, 10, "", "")
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}

	if entries[0].Subsystem != "store" || entries[1].Level != LevelError {
		t.Fatalf("entries = %+v", entries)
	}
}

// TestRedactNeverPanicsOnHostileInput guards the redaction hot path
// against malformed input.
func TestRedactNeverPanicsOnHostileInput(t *testing.T) {
	hostile := []string{
		"",
		"://",
		"vless://",
		strings.Repeat("://", 10_000),
		"uuid " + strings.Repeat("a", 32) + "-b",
		fmt.Sprintf("password=%s", strings.Repeat("p", 1<<16)),
		"\x00\x01\xff vmess://@",
	}

	for _, input := range hostile {
		_ = Redact(input)
	}
}

// TestRedactProtocolURLsAdversarial verifies every credential-bearing
// protocol URL family is redacted — VLESS, VMess, Trojan, Shadowsocks
// (ss), Hysteria2, TUIC, Juicity and Naive+HTTPS. The credential
// portion (UUID, password, base64 userinfo) must NEVER survive
// redaction, while the scheme prefix stays useful for diagnostics.
func TestRedactProtocolURLsAdversarial(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		mustGo  string // must contain
		mustNot string // must NOT contain
	}{
		{
			name:    "vless uuid",
			input:   "vless://a3f5b8e2-1c4d-4e2a-9f8b-7c6d5e4f3a2b@srv.example.com:443?type=ws",
			mustGo:  "vless://[REDACTED]",
			mustNot: "a3f5b8e2-1c4d-4e2a-9f8b-7c6d5e4f3a2b",
		},
		{
			name:    "vmess base64",
			input:   "vmess://eyJ2IjoiMiIsInBzIjoibm9kZS1hIn0=@srv.example.com:443?security=tls",
			mustGo:  "vmess://[REDACTED]",
			mustNot: "eyJ2IjoiMiIsInBzIjoibm9kZS1hIn0",
		},
		{
			name:    "trojan password",
			input:   "trojan://secretpass123@srv.example.com:443#node-name",
			mustGo:  "trojan://[REDACTED]",
			mustNot: "secretpass123",
		},
		{
			name:    "shadowsocks password",
			input:   "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQxMjM=@srv.example.com:8388",
			mustGo:  "ss://[REDACTED]",
			mustNot: "YWVzLTI1Ni1nY206cGFzc3dvcmQxMjM",
		},
		{
			name:    "hysteria2 password",
			input:   "hysteria2://sup3rs3cr3t@srv.example.com:443?sni=example.com",
			mustGo:  "hysteria2://[REDACTED]",
			mustNot: "sup3rs3cr3t",
		},
		{
			name:    "hysteria password",
			input:   "hysteria://authkey456@srv.example.com:443",
			mustGo:  "hysteria://[REDACTED]",
			mustNot: "authkey456",
		},
		{
			name:    "tuic uuid+password",
			input:   "tuic://b8e3f2a1-4c5d-6e7f-8a9b-0c1d2e3f4a5b:tuicpass@srv.example.com:443",
			mustGo:  "tuic://[REDACTED]",
			mustNot: "b8e3f2a1-4c5d-6e7f-8a9b-0c1d2e3f4a5b",
		},
		{
			name:    "juicity uuid+password",
			input:   "juicity://c1d2e3f4-a5b6-4c7d-8e9f-0a1b2c3d4e5f:juicitypass@srv:443",
			mustGo:  "juicity://[REDACTED]",
			mustNot: "c1d2e3f4-a5b6-4c7d-8e9f-0a1b2c3d4e5f",
		},
		{
			name:    "naive+https",
			input:   "naive+https://user:pass@srv.example.com:443",
			mustGo:  "naive+https://[REDACTED]",
			mustNot: "pass",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Redact(tc.input)

			if !strings.Contains(out, tc.mustGo) {
				t.Fatalf("redaction of %q = %q, must contain %q",
					tc.input, out, tc.mustGo)
			}

			if strings.Contains(out, tc.mustNot) {
				t.Fatalf("redaction of %q leaked %q: %q",
					tc.input, tc.mustNot, out)
			}
		})
	}
}

// TestRedactJSONKeyValueSecrets verifies JSON-shaped and key=value
// secret parameters are redacted across every recognized key name.
func TestRedactJSONKeyValueSecrets(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		mustNot string
	}{
		{"password=json", `{"password":"hunter2"}`, "hunter2"},
		{"passwd=json", `{"passwd":"secret123"}`, "secret123"},
		{"pwd=json", `{"pwd":"abc"}`, "abc"},
		{"token=json", `{"token":"tok-xyz"}`, "tok-xyz"},
		{"secret=json", `{"secret":"s3cr3t"}`, "s3cr3t"},
		{"api_key=json", `{"api_key":"ak-123"}`, "ak-123"},
		{"api-key=json", `{"api-key":"ak-456"}`, "ak-456"},
		{"private_key=json", `{"private_key":"pk-xyz"}`, "pk-xyz"},
		{"private-key=json", `{"private-key":"pk-abc"}`, "pk-abc"},
		{"auth=json", `{"auth":"bearer-xyz"}`, "bearer-xyz"},
		{"authorization=json", `{"authorization":"Basic abc"}`, "Basic abc"},
		{"password=kv", "password=hunter2", "hunter2"},
		{"passwd:kv", "passwd: secret123", "secret123"},
		{"token = kv", "token = tok-xyz", "tok-xyz"},
		{"api_key:kv", "api_key: ak-123", "ak-123"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Redact(tc.input)

			if strings.Contains(out, tc.mustNot) {
				t.Fatalf("redaction of %q leaked %q: %q",
					tc.input, tc.mustNot, out)
			}
		})
	}
}

// TestRedactURLQuerySecrets verifies secret-bearing URL query
// parameters are redacted while benign parameters survive.
func TestRedactURLQuerySecrets(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		mustGo  string
		mustNot string
	}{
		{"password query", "https://ex.com/?password=secret&page=2", "[REDACTED]", "secret"},
		{"token query", "https://ex.com/?token=tok123&x=1", "[REDACTED]", "tok123"},
		{"secret query", "https://ex.com/?secret=s&y=2", "[REDACTED]", "secret=s"},
		{"key query", "https://ex.com/?key=k123", "[REDACTED]", "k123"},
		{"auth query", "https://ex.com/?auth=a1", "[REDACTED]", "a1"},
		{"benign query", "https://ex.com/?page=2&sort=asc", "page=2", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Redact(tc.input)

			if tc.mustNot != "" && strings.Contains(out, tc.mustNot) {
				t.Fatalf("redaction of %q leaked %q: %q",
					tc.input, tc.mustNot, out)
			}

			if tc.mustGo != "" && !strings.Contains(out, tc.mustGo) {
				t.Fatalf("redaction of %q = %q, must contain %q",
					tc.input, out, tc.mustGo)
			}
		})
	}
}

// TestRedactExplicitSecrets verifies caller-registered secret values
// are replaced verbatim before pattern redaction runs.
func TestRedactExplicitSecrets(t *testing.T) {
	secrets := []string{
		"super-secret-value",
		"another-secret",
	}

	out := RedactWithSecrets(
		"connect to super-secret-value then another-secret",
		secrets,
	)

	if strings.Contains(out, "super-secret-value") {
		t.Fatalf("first explicit secret survived: %s", out)
	}

	if strings.Contains(out, "another-secret") {
		t.Fatalf("second explicit secret survived: %s", out)
	}

	// Empty secrets are skipped (no replacement of empty string).
	empty := RedactWithSecrets("text", []string{""})
	if empty != "text" {
		t.Fatalf("empty secret should be skipped: %q", empty)
	}
}

// TestRedactUUIDs verifies bare UUID-shaped values are redacted.
func TestRedactUUIDs(t *testing.T) {
	uuids := []string{
		"11111111-1111-1111-1111-111111111111",
		"a3f5b8e2-1c4d-4e2a-9f8b-7c6d5e4f3a2b",
		"ABCDEF12-3456-7890-ABCD-EF1234567890",
	}

	for _, uuid := range uuids {
		out := Redact("config id " + uuid)
		if strings.Contains(out, uuid) {
			t.Fatalf("UUID %q survived redaction: %s", uuid, out)
		}
	}
}
