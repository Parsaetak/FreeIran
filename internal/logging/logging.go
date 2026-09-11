// Package logging implements FreeIran's persistent runtime log: a
// structured, machine-readable, size-rotated, redaction-enforcing
// event stream shared by every subsystem (app, store, migration,
// core manager, connection manager, tester, system layer).
//
// Design:
//
//   - Entries are JSON lines with timestamp, level, subsystem, event,
//     message and optional operation / error-category fields — every
//     runtime log entry carries the full §17 shape.
//   - The log lives in the platform application-data directory
//     (<BaseDir>/logs/freeiran.log); it is never written into the
//     repository.
//   - Rotation is size-based with a bounded backup count; rotation
//     is crash-safe (plain sequential renames) and recovers cleanly
//     after an interrupted rotation.
//   - EVERY entry passes through redaction before it is stored,
//     broadcast or written: UUIDs, credentials in protocol URLs,
//     key/password/token parameters and any explicitly registered
//     secrets are replaced. Core stdout/stderr must only ever reach
//     the log through this layer.
//   - A bounded in-memory ring mirrors the file for the UI diagnostics
//     view (incremental polling by sequence number, never a full-file
//     transfer) and subscribers receive new entries without polling.
//   - The package-level global logger makes integration possible
//     from low-level engine packages without dependency cycles; with
//     no global installed, all functions are no-ops, so libraries
//     stay silent by default (and in unit tests).
package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level is a log severity.
type Level string

// Severity levels in increasing order.
const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// severity ranks levels for filtering.
func severity(l Level) int {
	switch l {
	case LevelDebug:
		return 0
	case LevelInfo:
		return 1
	case LevelWarn:
		return 2
	case LevelError:
		return 3
	}

	return 1
}

// Entry is one structured runtime log record (§17).
type Entry struct {
	Seq       uint64 `json:"seq"`
	Time      string `json:"ts"` // RFC3339, UTC
	Level     Level  `json:"level"`
	Subsystem string `json:"subsystem"`
	Event     string `json:"event"`
	Message   string `json:"message"`
	Operation string `json:"operation,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
}

// Options configure the log file.
type Options struct {
	// Dir is the log directory (created on demand).
	Dir string

	// Name is the base file name (default freeiran.log).
	Name string

	// MaxBytes bounds the primary file before rotation
	// (default 5 MiB).
	MaxBytes int64

	// MaxBackups bounds kept rotated files (default 4).
	MaxBackups int

	// MinLevel filters entries below the level (default info).
	MinLevel Level

	// MirrorStderr additionally writes entries to stderr (dev mode).
	MirrorStderr bool
}

// ringCapacity bounds the in-memory mirror. Entries are cheap and
// the UI reads incrementally; 2048 entries cover the recent activity
// surface generously.
const ringCapacity = 2048

// Logger writes structured, redacted entries to a rotated file, a
// bounded memory ring and optional subscribers.
type Logger struct {
	opts Options

	mu      sync.Mutex
	file    *os.File
	written int64
	seq     atomic.Uint64
	ring    []Entry

	subMu   sync.RWMutex
	subs    map[chan Entry]struct{}
	dropped atomic.Uint64
}

// Open creates the log directory, recovers from an interrupted
// rotation and returns a ready logger.
func Open(opts Options) (*Logger, error) {
	if opts.Name == "" {
		opts.Name = "freeiran.log"
	}

	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 5 << 20 // 5 MiB
	}

	if opts.MaxBackups <= 0 {
		opts.MaxBackups = 4
	}

	if opts.MinLevel == "" {
		opts.MinLevel = LevelInfo
	}

	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}

	l := &Logger{
		opts: opts,
		ring: make([]Entry, 0, 256),
		subs: make(map[chan Entry]struct{}),
	}

	if err := l.recoverRotation(); err != nil {
		return nil, err
	}

	if err := l.openFile(); err != nil {
		return nil, err
	}

	return l, nil
}

// path is the primary log file path.
func (l *Logger) path() string {
	return filepath.Join(l.opts.Dir, l.opts.Name)
}

// recoverRotation repairs the file set after a crash mid-rotation:
// rotation only uses sequential renames, so a partially completed
// rotation leaves ordinary numbered backups behind — the next
// rotation simply continues the shift. An oversized primary file
// from an interrupted session is rotated immediately so the new
// session starts clean.
func (l *Logger) recoverRotation() error {
	info, err := os.Stat(l.path())
	if err == nil && info.Size() > l.opts.MaxBytes {
		return shiftBackups(l.opts.Dir, l.opts.Name, l.opts.MaxBackups)
	}

	return nil
}

// shiftBackups renames log.N-1 → log.N ... log → log.1.
func shiftBackups(dir, name string, backups int) error {
	// Drop the oldest backup.
	oldest := filepath.Join(dir, name+"."+itoa(backups))

	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return err
	}

	for i := backups - 1; i >= 1; i-- {
		from := filepath.Join(dir, name+"."+itoa(i))
		to := filepath.Join(dir, name+"."+itoa(i+1))

		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return os.Rename(filepath.Join(dir, name), filepath.Join(dir, name+".1"))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}

	digits := ""

	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}

	return digits
}

// openFile opens (or reopens) the primary log file for appending.
func (l *Logger) openFile() error {
	file, err := os.OpenFile(l.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()

		return err
	}

	l.file = file
	l.written = info.Size()

	return nil
}

// rotate closes the current file, shifts backups and reopens.
// Callers hold l.mu.
func (l *Logger) rotate() error {
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			return err
		}

		l.file = nil
	}

	if err := shiftBackups(l.opts.Dir, l.opts.Name, l.opts.MaxBackups); err != nil {
		return err
	}

	return l.openFile()
}

// SetLevel adjusts the minimum level at runtime (settings UI).
func (l *Logger) SetLevel(level Level) {
	if level == "" {
		return
	}

	l.mu.Lock()
	l.opts.MinLevel = level
	l.mu.Unlock()
}

// SetLimits adjusts rotation limits at runtime (settings UI). The
// next write applies them.
func (l *Logger) SetLimits(maxBytes int64, maxBackups int) {
	if maxBytes <= 0 || maxBackups <= 0 {
		return
	}

	l.mu.Lock()
	l.opts.MaxBytes = maxBytes
	l.opts.MaxBackups = maxBackups
	l.mu.Unlock()
}

// Info logs an informational lifecycle event.
func (l *Logger) Info(subsystem, event, format string, args ...any) {
	l.write(LevelInfo, subsystem, event, "", "", format, args...)
}

// Warn logs a warning event.
func (l *Logger) Warn(subsystem, event, format string, args ...any) {
	l.write(LevelWarn, subsystem, event, "", "", format, args...)
}

// Error logs an error event with optional operation and error kind.
func (l *Logger) Error(subsystem, event, operation, errKind, format string, args ...any) {
	l.write(LevelError, subsystem, event, operation, errKind, format, args...)
}

// Debug logs a diagnostic event.
func (l *Logger) Debug(subsystem, event, format string, args ...any) {
	l.write(LevelDebug, subsystem, event, "", "", format, args...)
}

// write builds, redacts, stores and broadcasts one entry.
func (l *Logger) write(
	level Level,
	subsystem, event, operation, errKind, format string, args ...any,
) {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}

	entry := Entry{
		Seq:       l.seq.Add(1),
		Time:      time.Now().UTC().Format(time.RFC3339Nano),
		Level:     level,
		Subsystem: subsystem,
		Event:     event,
		Message:   Redact(message),
		Operation: operation,
		ErrorKind: errKind,
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Level filter is checked under the lock so SetLevel races are
	// serialized with writes.
	if severity(level) < severity(l.opts.MinLevel) {
		return
	}

	// Ring buffer (bounded, drop-oldest in halves like the WAL).
	l.ring = append(l.ring, entry)
	if len(l.ring) > ringCapacity {
		l.ring = append([]Entry(nil), l.ring[ringCapacity/2:]...)
	}

	// File write + rotation.
	if l.file != nil {
		if raw, err := json.Marshal(entry); err == nil {
			raw = append(raw, '\n')

			if l.written+int64(len(raw)) > l.opts.MaxBytes {
				if rotateErr := l.rotate(); rotateErr == nil {
					l.writeRotationNote()
				}
			}

			if l.file != nil {
				if n, wErr := l.file.WriteString(string(raw)); wErr == nil {
					l.written += int64(n)
				}
			}
		}
	}

	if l.opts.MirrorStderr {
		raw, err := json.Marshal(entry)
		if err == nil {
			_, _ = os.Stderr.WriteString(string(raw) + "\n")
		}
	}

	l.subMu.RLock()

	for ch := range l.subs {
		select {
		case ch <- entry:
		default:
			l.dropped.Add(1) // slow subscriber: drop, never block
		}
	}

	l.subMu.RUnlock()
}

// writeRotationNote records a rotation boundary in the fresh file.
func (l *Logger) writeRotationNote() {
	if l.file == nil {
		return
	}

	note := Entry{
		Seq:       l.seq.Add(1),
		Time:      time.Now().UTC().Format(time.RFC3339Nano),
		Level:     LevelInfo,
		Subsystem: "logging",
		Event:     "log_rotated",
		Message:   "runtime log rotated; new file started",
	}

	if raw, err := json.Marshal(note); err == nil {
		raw = append(raw, '\n')

		if n, wErr := l.file.WriteString(string(raw)); wErr == nil {
			l.written += int64(n)
		}
	}
}

// Recent returns up to limit entries newer than sinceSeq, filtered by
// optional subsystem and substring query (case-insensitive). The
// result is ordered by sequence — the UI polls incrementally and
// never transfers the whole buffer.
func (l *Logger) Recent(sinceSeq uint64, limit int, subsystem, query string) []Entry {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	query = strings.ToLower(query)

	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]Entry, 0, 32)

	for _, entry := range l.ring {
		if entry.Seq <= sinceSeq {
			continue
		}

		if subsystem != "" && entry.Subsystem != subsystem {
			continue
		}

		if query != "" &&
			!strings.Contains(strings.ToLower(entry.Message), query) &&
			!strings.Contains(strings.ToLower(entry.Event), query) {
			continue
		}

		out = append(out, entry)

		if len(out) >= limit {
			break
		}
	}

	return out
}

// LastSeq returns the highest sequence number written so far.
func (l *Logger) LastSeq() uint64 {
	return l.seq.Load()
}

// Clear drops the in-memory ring (the UI "clear display" action). The
// on-disk log is preserved for diagnostics.
func (l *Logger) Clear() {
	l.mu.Lock()
	l.ring = l.ring[:0]
	l.mu.Unlock()
}

// Subscribe registers a channel receiving live entries (buffered;
// slow subscribers drop instead of blocking the engine).
func (l *Logger) Subscribe(buffer int) (<-chan Entry, func()) {
	if buffer <= 0 {
		buffer = 256
	}

	ch := make(chan Entry, buffer)

	l.subMu.Lock()
	l.subs[ch] = struct{}{}
	l.subMu.Unlock()

	cancel := func() {
		l.subMu.Lock()
		delete(l.subs, ch)
		l.subMu.Unlock()
	}

	return ch, cancel
}

// Close flushes and closes the log file. Entries logged after Close
// are kept in the ring only (shutdown ordering: the logger closes
// last, after the store and the connection manager).
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return nil
	}

	err := l.file.Sync()
	if closeErr := l.file.Close(); err == nil {
		err = closeErr
	}

	l.file = nil

	return err
}

// global hosts the process-wide logger for engine packages.
var global atomic.Pointer[Logger]

// SetGlobal installs the process-wide logger.
func SetGlobal(l *Logger) { global.Store(l) }

// Global returns the process-wide logger (nil before installation).
func Global() *Logger { return global.Load() }

// E logs an info event through the global logger (no-op when unset).
func E(subsystem, event, format string, args ...any) {
	if l := global.Load(); l != nil {
		l.Info(subsystem, event, format, args...)
	}
}

// W logs a warning event through the global logger (no-op when unset).
func W(subsystem, event, format string, args ...any) {
	if l := global.Load(); l != nil {
		l.Warn(subsystem, event, format, args...)
	}
}

// Err logs an error event through the global logger (no-op when unset).
func Err(subsystem, event, operation, errKind, format string, args ...any) {
	if l := global.Load(); l != nil {
		l.Error(subsystem, event, operation, errKind, format, args...)
	}
}

// D logs a debug event through the global logger (no-op when unset).
func D(subsystem, event, format string, args ...any) {
	if l := global.Load(); l != nil {
		l.Debug(subsystem, event, format, args...)
	}
}

// --- Redaction -------------------------------------------------------

// Redaction patterns (§19). Applied to EVERY entry before storage,
// broadcast and file write.
var (
	// protocolURLs matches credential-bearing protocol URLs in full.
	protocolURLs = regexp.MustCompile(
		`(?i)\b(vless|vmess|trojan|ss|hysteria2?|tuic|juicity|naive\+https?)://[^\s"'\]]+`)

	// uuidPattern matches UUID-shaped values (never log one bare).
	uuidPattern = regexp.MustCompile(
		`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

	// keyValues matches key=value / key: value secret parameters.
	keyValues = regexp.MustCompile(
		`(?i)\b(password|passwd|pwd|token|secret|api[_-]?key|private[_-]?key|auth|authorization)` +
			`(\s*[=:]\s*|\s*":\s*")([^\s",}&]+)`)

	// urlQuerySecrets matches secret-bearing URL query parameters.
	urlQuerySecrets = regexp.MustCompile(
		`(?i)([?&](password|token|secret|key|auth|id|uuid)=)[^&\s]+`)
)

const redactedMark = "[REDACTED]"

// Redact removes credential material from diagnostic text. Patterns
// run from most to least specific; explicit secrets registered
// through RedactWithSecrets are replaced first.
func Redact(text string) string {
	return RedactWithSecrets(text, nil)
}

// RedactWithSecrets redacts pattern matches plus the exact secret
// values provided by the caller (config credential fields, core
// output capture, etc.).
func RedactWithSecrets(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}

		text = strings.ReplaceAll(text, secret, redactedMark)
	}

	text = protocolURLs.ReplaceAllStringFunc(text, func(match string) string {
		schemeEnd := strings.Index(match, "://")
		if schemeEnd < 0 {
			return redactedMark
		}

		return match[:schemeEnd+3] + redactedMark
	})

	text = urlQuerySecrets.ReplaceAllString(text, "$1"+redactedMark)
	text = keyValues.ReplaceAllString(text, "$1$2"+redactedMark)
	text = uuidPattern.ReplaceAllString(text, redactedMark)

	return text
}
