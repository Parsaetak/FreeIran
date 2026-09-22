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
//   - v0.9.7 session identity and correlation: every launch gets a
//     unique session_id; every entry carries a monotonic per-session
//     sequence and a unique event_id; causal debugging uses the
//     parent_event_id / batch_id / test_id / config_id / core
//     correlation fields. Important structured values never hide
//     inside the message text — they ride the first-class fields or
//     the generic Fields map.
//   - v0.9.7 noise control: the Dedupe helper collapses repetitive
//     events (per-process fallbacks, routine launches) into one
//     session-level record plus an aggregated counter, and lifecycle
//     events use distinct event names (application_start,
//     workspace_ready, services_ready, application_ready,
//     background_warmup, application_shutdown) so base-path
//     initialization never produces duplicate start messages.
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
	"crypto/rand"
	"encoding/hex"
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

// Profile is the logging-profile policy (v0.9.8.4, roadmap P1 §15):
// ONE authoritative classification of how much the runtime log
// carries. The profile — not scattered `if debug` checks in the
// subsystems — decides which semantic records the logger admits:
//
//	Normal    default end-user mode: important startup/shutdown,
//	          connection lifecycle, verification, provider/core
//	          lifecycle, failures, recovery, meaningful memory and
//	          installation events. Routine verbose diagnostics are
//	          suppressed; records stay compact; no unnecessary
//	          correlation identifiers.
//	Detailed  everything Normal keeps, plus lifecycle-tagged
//	          diagnostic records and session/event identity on every
//	          emitted record (full lifecycle traceability). No
//	          uncontrolled high-frequency spam: the dedupe and
//	          admission gates still apply.
//	Debug     verbose diagnostic records, detailed subsystem fields,
//	          correlation IDs on every record (connection/recovery
//	          episode identifiers become universally available). More
//	          logs by design — still bounded by rotation and
//	          retention.
type Profile string

const (
	ProfileNormal   Profile = "normal"
	ProfileDetailed Profile = "detailed"
	ProfileDebug    Profile = "debug"
)

// ParseProfile maps a settings string onto a logging profile (""
// and unknown values fall back to Normal — the safe default).
func ParseProfile(raw string) Profile {
	switch Profile(raw) {
	case ProfileDetailed:
		return ProfileDetailed
	case ProfileDebug:
		return ProfileDebug
	default:
		return ProfileNormal
	}
}

// Valid reports whether raw names a real profile (settings
// validation).
func (p Profile) Valid() bool {
	return p == ProfileNormal || p == ProfileDetailed || p == ProfileDebug
}

// profileAdmits is the profile half of the admission policy: the
// severity half (the user's minimum level) is applied separately.
func profileAdmits(profile Profile, level Level, lifecycle bool) bool {
	if level != LevelDebug {
		return true
	}

	switch profile {
	case ProfileDebug:
		return true
	case ProfileDetailed:
		// Only records the subsystems marked as lifecycle-relevant
		// (connection/provider/core lifecycle detail) — routine
		// verbose diagnostics stay suppressed.
		return lifecycle
	default:
		return false
	}
}

// admitsPolicy is the complete admission policy in ONE place
// (v0.9.8.4): debug-severity verbosity is governed ENTIRELY by the
// profile (the severity floor governs info/warn/error records), so
// Normal stays compact regardless of a legacy debug floor, Detailed
// adds exactly its lifecycle tier and Debug unlocks everything.
func admitsPolicy(profile Profile, minLevel Level, level Level, lifecycle bool) bool {
	if level == LevelDebug {
		return profileAdmits(profile, level, lifecycle)
	}

	return severity(level) >= severity(minLevel)
}

// Entry is one structured runtime log record (§17 + v0.9.7 session
// identity and correlation fields).
type Entry struct {
	Seq       uint64 `json:"seq"`
	Time      string `json:"ts"` // RFC3339, UTC
	Level     Level  `json:"level"`
	Subsystem string `json:"subsystem"`
	Event     string `json:"event"`
	Message   string `json:"message"`
	Operation string `json:"operation,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`

	// Session identity (v0.9.7): session_id identifies one
	// application launch; Seq is monotonic within the session and
	// EventID is unique per entry. Restarts are distinguishable by
	// session_id alone — no message parsing required.
	// v0.9.8.3: identity is emitted ONLY on correlated records.
	SessionID string `json:"session_id,omitempty"`
	EventID   string `json:"event_id,omitempty"`

	// Correlated marks a record as correlation-opt-in (not
	// serialized; drives identity assignment in emit).
	Correlated bool `json:"-"`

	// Lifecycle marks a debug-severity record as lifecycle-relevant
	// (not serialized; admitted by the Detailed profile, which
	// suppresses routine verbose diagnostics).
	Lifecycle bool `json:"-"`

	// Correlation (v0.9.7): parent_event_id links an event to the
	// event that caused it; batch_id groups a bulk test run;
	// test_id / config_id tie events to one test / one
	// configuration; core + pid + listener identify a core
	// process; duration_ms / status carry outcome data.
	ParentEventID string         `json:"parent_event_id,omitempty"`
	BatchID       string         `json:"batch_id,omitempty"`
	TestID        string         `json:"test_id,omitempty"`
	ConfigID      string         `json:"config_id,omitempty"`
	Core          string         `json:"core,omitempty"`
	PID           int            `json:"pid,omitempty"`
	Listener      string         `json:"listener,omitempty"`
	DurationMS    int64          `json:"duration_ms,omitempty"`
	Status        string         `json:"status,omitempty"`
	Fields        map[string]any `json:"fields,omitempty"`
}

// Record is the structured-input twin of Entry (v0.9.7). Callers fill
// what they know; Log redacts message text and string field values
// before storage. v0.9.8.3: session/event identity is assigned ONLY
// when Correlate is set (or the record already carries correlation
// ids) — ordinary records stay compact.
type Record struct {
	Level      Level
	Subsystem  string
	Event      string
	Message    string
	Operation  string
	ErrorKind  string
	ParentID   string
	BatchID    string
	TestID     string
	ConfigID   string
	Core       string
	PID        int
	Listener   string
	DurationMS int64
	Status     string
	Fields     map[string]any

	// Correlate stamps the record with session + event identity for
	// multi-event investigations (connection/recovery episodes,
	// startup session, explicit debug). Records that already carry
	// ParentID/BatchID/TestID are always correlated.
	Correlate bool

	// Lifecycle marks a debug-severity record as lifecycle-relevant
	// (connection/provider/core lifecycle detail): admitted by the
	// Detailed profile, suppressed by Normal, universal in Debug.
	Lifecycle bool
}

// newSessionID returns a random 16-hex-char session identifier.
func newSessionID() string {
	var buf [8]byte

	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing is catastrophic but must not panic
		// the logger; fall back to a time-derived id.
		return fmt.Sprintf("%016x", time.Now().UTC().UnixNano())
	}

	return hex.EncodeToString(buf[:])
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

	// MaxAgeDays bounds retained log files by age: backups older
	// than this are deleted on open and after each rotation
	// (default 7; 0 = default). Debug deployments may raise it.
	MaxAgeDays int

	// MinLevel filters entries below the level (default info).
	// Debug-severity verbosity is governed by Profile, not by this
	// floor (v0.9.8.4).
	MinLevel Level

	// Profile is the logging-profile policy (default Normal).
	Profile Profile

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

	// session identifies the application launch (v0.9.7): every
	// entry carries it and the per-session monotonic sequence.
	session string

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

	if opts.MaxAgeDays <= 0 {
		opts.MaxAgeDays = 7
	}

	if opts.MinLevel == "" {
		opts.MinLevel = LevelInfo
	}

	if opts.Profile == "" {
		opts.Profile = ProfileNormal
	}

	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}

	l := &Logger{
		opts:    opts,
		session: newSessionID(),
		ring:    make([]Entry, 0, 256),
		subs:    make(map[chan Entry]struct{}),
	}

	if err := l.recoverRotation(); err != nil {
		return nil, err
	}

	if err := l.openFile(); err != nil {
		return nil, err
	}

	// Age-based retention sweep at startup (bounded disk usage).
	l.pruneRetention()

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

// SetProfile switches the logging-profile policy at runtime (settings
// UI). The switch takes effect immediately — no restart. Unknown
// profiles are ignored (the previous policy stays authoritative).
func (l *Logger) SetProfile(profile Profile) {
	if !profile.Valid() {
		return
	}

	l.mu.Lock()
	l.opts.Profile = profile
	l.mu.Unlock()
}

// Profile returns the active logging-profile policy.
func (l *Logger) Profile() Profile {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.opts.Profile
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

// SetMaxAgeDays adjusts the age-based retention bound at runtime
// (settings UI). Values below 1 keep the previous default.
func (l *Logger) SetMaxAgeDays(days int) {
	if days < 1 {
		days = 7
	}

	l.mu.Lock()
	l.opts.MaxAgeDays = days
	l.mu.Unlock()
}

// pruneRetention deletes rotated backups older than MaxAgeDays
// (bounded disk activity: age, count and size are all bounded).
func (l *Logger) pruneRetention() {
	dir, name := l.opts.Dir, l.opts.Name

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-time.Duration(l.opts.MaxAgeDays) * 24 * time.Hour)

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), name+".") || entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}

		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
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

// looksSecretKey reports whether a structured field key names a
// secret (password/token/key/auth family) so its value must never be
// stored verbatim.
func looksSecretKey(key string) bool {
	lowered := strings.ToLower(key)

	for _, fragment := range []string{
		"password", "passwd", "pwd", "token", "secret",
		"api_key", "apikey", "private_key", "credential",
		"authorization", "cookie", "session_key",
	} {
		if strings.Contains(lowered, fragment) {
			return true
		}
	}

	return false
}

// SessionID returns the unique identifier of this application launch.
func (l *Logger) SessionID() string { return l.session }

// Log writes one structured record (v0.9.8.3): the level filter runs
// FIRST (suppressed records pay no formatting/redaction cost),
// message and string field values are redacted, and identity fields
// are assigned ONLY to correlation-opt-in records (connection and
// recovery episodes, startup session, explicit debug). When
// rec.Message is empty the Event name is used so records never lose
// their human summary line.
func (l *Logger) Log(rec Record) {
	// Fast path (v0.9.8.3/v0.9.8.4): a suppressed record must not
	// build an Entry, redact strings or copy field maps — idle
	// logging overhead stays near zero. The severity floor and the
	// profile policy are checked BEFORE any formatting cost.
	l.mu.Lock()
	profile, minLevel := l.opts.Profile, l.opts.MinLevel
	l.mu.Unlock()

	if !admitsPolicy(profile, minLevel, rec.Level, rec.Lifecycle) {
		return
	}

	message := rec.Message
	if message == "" {
		message = rec.Event
	}

	entry := Entry{
		Level:         rec.Level,
		Subsystem:     rec.Subsystem,
		Event:         rec.Event,
		Message:       Redact(message),
		Operation:     rec.Operation,
		ErrorKind:     rec.ErrorKind,
		ParentEventID: rec.ParentID,
		BatchID:       rec.BatchID,
		TestID:        rec.TestID,
		ConfigID:      rec.ConfigID,
		Core:          rec.Core,
		PID:           rec.PID,
		Listener:      rec.Listener,
		DurationMS:    rec.DurationMS,
		Status:        rec.Status,
		Correlated:    rec.Correlate || rec.ParentID != "" || rec.BatchID != "" || rec.TestID != "",
		Lifecycle:     rec.Lifecycle,
	}

	if len(rec.Fields) > 0 {
		fields := make(map[string]any, len(rec.Fields))

		for key, value := range rec.Fields {
			// Secret-shaped keys never store raw values;
			// other string values pass through redaction.
			if looksSecretKey(key) {
				value = redactedMark
			} else if s, ok := value.(string); ok {
				value = Redact(s)
			}

			fields[key] = value
		}

		entry.Fields = fields
	}

	l.emit(entry)
}

// write builds, redacts, stores and broadcasts one entry (level
// filtered first — suppressed records cost nothing).
func (l *Logger) write(
	level Level,
	subsystem, event, operation, errKind, format string, args ...any,
) {
	l.mu.Lock()
	profile, minLevel := l.opts.Profile, l.opts.MinLevel
	l.mu.Unlock()

	if !admitsPolicy(profile, minLevel, level, false) {
		return
	}

	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}

	l.emit(Entry{
		Level:     level,
		Subsystem: subsystem,
		Event:     event,
		Message:   Redact(message),
		Operation: operation,
		ErrorKind: errKind,
	})
}

// DebugLifecycle logs a lifecycle-relevant diagnostic record
// (v0.9.8.4): suppressed by Normal, admitted by Detailed and Debug.
// Subsystems use it for connection/provider/core lifecycle detail —
// never for high-frequency routine diagnostics.
func (l *Logger) DebugLifecycle(subsystem, event, format string, args ...any) {
	l.mu.Lock()
	profile, minLevel := l.opts.Profile, l.opts.MinLevel
	l.mu.Unlock()

	if !admitsPolicy(profile, minLevel, LevelDebug, true) {
		return
	}

	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}

	l.emit(Entry{
		Level:     LevelDebug,
		Subsystem: subsystem,
		Event:     event,
		Message:   Redact(message),
		Lifecycle: true,
	})
}

// DL logs one lifecycle-relevant diagnostic record through the global
// logger (no-op when unset).
func DL(subsystem, event, format string, args ...any) {
	if l := global.Load(); l != nil {
		l.DebugLifecycle(subsystem, event, format, args...)
	}
}

// emit finalizes one entry (identity assignment for correlated
// records only, level filter, ring, file write with rotation,
// stderr mirror, subscriber broadcast).
func (l *Logger) emit(entry Entry) {
	// v0.9.8.3: session_id / event_id exist ONLY on correlated
	// records (connection/recovery episodes, startup session,
	// explicit debug investigations). Ordinary records stay
	// compact — no per-record identity allocations.
	// Seq stays on every record (the UI reads incrementally by
	// sequence); only the session/event identity is correlation-gated.
	entry.Seq = l.seq.Add(1)

	l.mu.Lock()

	// v0.9.8.4: identity stamping follows the profile policy.
	// Normal — correlation-opt-in records only (compact, no
	// per-record session/event identity). Detailed and Debug — every
	// emitted record carries session/event identity (lifecycle
	// traceability; correlation IDs universally available).
	if entry.Correlated || l.opts.Profile == ProfileDetailed || l.opts.Profile == ProfileDebug {
		entry.SessionID = l.session
		entry.EventID = fmt.Sprintf("%s-%04x", l.session[:8], entry.Seq)
	}

	entry.Time = time.Now().UTC().Format(time.RFC3339Nano)

	defer l.mu.Unlock()

	// Level AND profile filters are re-checked under the lock so
	// SetLevel/SetProfile races are serialized with writes (the
	// Log/write fast paths already dropped most suppressed records
	// before this point).
	if !admitsPolicy(l.opts.Profile, l.opts.MinLevel, entry.Level, entry.Lifecycle) {
		return
	}

	// Ring buffer (bounded, drop-oldest in halves like the WAL).
	l.ring = append(l.ring, entry)
	if len(l.ring) > ringCapacity {
		l.ring = append([]Entry(nil), l.ring[ringCapacity/2:]...)
	}

	// File write + rotation (single marshal reused for the mirror).
	if l.file != nil {
		if raw, err := json.Marshal(entry); err == nil {
			raw = append(raw, '\n')

			if l.written+int64(len(raw)) > l.opts.MaxBytes {
				if rotateErr := l.rotate(); rotateErr == nil {
					l.writeRotationNote()
					l.pruneRetention()
				}
			}

			if l.file != nil {
				if n, wErr := l.file.WriteString(string(raw)); wErr == nil {
					l.written += int64(n)
				}
			}

			if l.opts.MirrorStderr {
				_, _ = os.Stderr.Write(raw)
			}
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

// Filter selects log entries for the diagnostics view (v0.9.7):
// session, subsystem, minimum level, exact event, correlation ids
// (batch / test / config / core) and an errors-only shortcut join
// the original substring query.
type Filter struct {
	SinceSeq   uint64
	Limit      int
	Subsystem  string
	Level      Level // minimum severity (empty = no floor)
	Event      string
	BatchID    string
	TestID     string
	ConfigID   string
	Core       string
	ErrorsOnly bool
	Query      string
}

// matches reports whether entry satisfies the filter.
func (f Filter) matches(e Entry) bool {
	if e.Seq <= f.SinceSeq {
		return false
	}

	if f.Subsystem != "" && e.Subsystem != f.Subsystem {
		return false
	}

	if f.Level != "" && severity(e.Level) < severity(f.Level) {
		return false
	}

	if f.Event != "" && e.Event != f.Event {
		return false
	}

	if f.BatchID != "" && e.BatchID != f.BatchID {
		return false
	}

	if f.TestID != "" && e.TestID != f.TestID {
		return false
	}

	if f.ConfigID != "" && e.ConfigID != f.ConfigID {
		return false
	}

	if f.Core != "" && e.Core != f.Core {
		return false
	}

	if f.ErrorsOnly && severity(e.Level) < severity(LevelWarn) {
		return false
	}

	if f.Query != "" {
		query := strings.ToLower(f.Query)

		if !strings.Contains(strings.ToLower(e.Message), query) &&
			!strings.Contains(strings.ToLower(e.Event), query) {
			return false
		}
	}

	return true
}

// Query returns up to filter.Limit entries matching the structured
// filter, ordered by sequence. Zero-limit defaults to 200; the hard
// cap matches Recent.
func (l *Logger) Query(filter Filter) []Entry {
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]Entry, 0, 32)

	for _, entry := range l.ring {
		if !filter.matches(entry) {
			continue
		}

		out = append(out, entry)

		if len(out) >= limit {
			break
		}
	}

	return out
}

// Related returns every ring entry correlated with the given event:
// the event itself, its children (parent_event_id = event_id) and all
// entries sharing its batch / test / config identity. This powers the
// "show related events" diagnostics action (v0.9.7 §20).
func (l *Logger) Related(eventID string, limit int) []Entry {
	if eventID == "" {
		return nil
	}

	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var anchor *Entry

	for i := range l.ring {
		if l.ring[i].EventID == eventID {
			anchor = &l.ring[i]

			break
		}
	}

	if anchor == nil {
		return nil
	}

	out := make([]Entry, 0, 8)

	for _, entry := range l.ring {
		correlated := entry.EventID == anchor.EventID ||
			entry.ParentEventID == anchor.EventID ||
			(anchor.BatchID != "" && entry.BatchID == anchor.BatchID) ||
			(anchor.TestID != "" && entry.TestID == anchor.TestID) ||
			(anchor.ConfigID != "" && entry.ConfigID == anchor.ConfigID)

		if !correlated {
			continue
		}

		out = append(out, entry)

		if len(out) >= limit {
			break
		}
	}

	return out
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

// ClearGlobalIfCurrent uninstalls the process-wide logger, but ONLY
// when it still points at l: the caller proves it owns the currently
// installed global before removing it. The app lifecycle uses this on
// every path that closes the process logger — failed construction and
// shutdown alike — so Global() can never keep pointing at a closed
// logger, while a global another owner legitimately installed in the
// meantime is never clobbered.
func ClearGlobalIfCurrent(l *Logger) {
	if l == nil {
		return
	}

	global.CompareAndSwap(l, nil)
}

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

// LogR writes one structured record through the global logger
// (no-op when unset). v0.9.7 structured entry point for low-level
// packages (core lifecycle, system supervision, discovery).
func LogR(rec Record) {
	if l := global.Load(); l != nil {
		l.Log(rec)
	}
}

// GlobalSessionID returns the session id of the installed global
// logger (empty when unset).
func GlobalSessionID() string {
	if l := global.Load(); l != nil {
		return l.SessionID()
	}

	return ""
}

// --- Noise control (v0.9.7) -------------------------------------------

// dedupeState tracks one suppression key.
type dedupeState struct {
	count     uint64
	logged    bool
	lastLogAt time.Time
}

// Dedupe collapses repetitive runtime events into one session-level
// record plus an aggregated counter. Expected-but-noisy conditions
// (per-process job fallbacks, routine temporary-core launches) log
// once and then stay silent; the counter is flushed as a compact
// summary event on demand (shutdown, diagnostics).
//
// A Dedupe is safe for concurrent use.
type Dedupe struct {
	mu    sync.Mutex
	state map[string]*dedupeState
}

// NewDedupe creates an empty suppression table.
func NewDedupe() *Dedupe {
	return &Dedupe{state: make(map[string]*dedupeState)}
}

// Do logs (key, format) at level the first time it is seen and
// silently counts every later occurrence. It reports whether this
// call was the first (logged) occurrence.
func (d *Dedupe) Do(
	l *Logger, level Level, subsystem, event, key, format string, args ...any,
) bool {
	if l == nil {
		if g := global.Load(); g != nil {
			l = g
		} else {
			return false
		}
	}

	d.mu.Lock()

	state := d.state[key]
	if state == nil {
		state = &dedupeState{}
		d.state[key] = state
	}

	state.count++

	first := !state.logged
	state.logged = true

	d.mu.Unlock()

	if first {
		message := format
		if len(args) > 0 {
			message = fmt.Sprintf(format, args...)
		}

		l.Log(Record{
			Level:     level,
			Subsystem: subsystem,
			Event:     event,
			Message:   message,
			Fields:    map[string]any{"dedupe_key": key},
		})
	}

	return first
}

// Count returns how many occurrences of key were seen (0 when none).
func (d *Dedupe) Count(key string) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()

	if state := d.state[key]; state != nil {
		return state.count
	}

	return 0
}

// Total returns the number of tracked keys.
func (d *Dedupe) Total() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return len(d.state)
}

// FlushSummary logs one aggregated event (event + "_summary") with
// per-key counters and optionally resets the table. Keys with a
// single occurrence are skipped when onlyMulti is set — the first
// occurrence was already logged in full.
func (d *Dedupe) FlushSummary(
	l *Logger, subsystem, event string, onlyMulti bool, reset bool,
) {
	if l == nil {
		if g := global.Load(); g != nil {
			l = g
		} else {
			return
		}
	}

	d.mu.Lock()

	counts := make(map[string]uint64, len(d.state))

	for key, state := range d.state {
		if !onlyMulti || state.count > 1 {
			counts[key] = state.count
		}
	}

	if reset {
		d.state = make(map[string]*dedupeState)
	}

	d.mu.Unlock()

	if len(counts) == 0 {
		return
	}

	l.Log(Record{
		Level:     LevelInfo,
		Subsystem: subsystem,
		Event:     event + "_summary",
		Message:   fmt.Sprintf("%d aggregated occurrence group(s)", len(counts)),
		Fields:    map[string]any{"counts": counts},
	})
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
