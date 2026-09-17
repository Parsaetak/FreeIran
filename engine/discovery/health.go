package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// health.go implements source intelligence (v0.9.6 §16): every
// configuration source — configured, trusted-public, search-discovered
// or content-derived — carries observable health, and the discovery
// engine prioritises healthy sources without permanently blacklisting
// a source for one temporary failure.
//
// Health is measured, never assumed:
//
//   - availability: fraction of recent fetches that connected at all;
//   - parse success: fraction of fetched bodies that parsed;
//   - valid yield: fraction of parsed candidates that validated;
//   - duplicate rate: fraction of candidates already known;
//   - latency: the measured fetch duration of the last success;
//   - freshness: time since the last successful fetch;
//   - failures/backoff: consecutive failures drive an exponential
//     backoff that CAPS at a ceiling (never a permanent blacklist).
//
// Persistence: the tracker's state is a single JSON document next to
// the engine's other state files; a corrupt file is discarded (health
// rebuilds from observation, it is never authoritative data).

// SourceHealth is the observable health of one source.
type SourceHealth struct {
	// SourceID is the stable source identifier.
	SourceID string `json:"source_id"`

	// Fetches counts every observed fetch attempt.
	Fetches int `json:"fetches"`

	// FetchOK counts fetches that connected and returned a body.
	FetchOK int `json:"fetch_ok"`

	// ParseOK counts bodies that parsed without error.
	ParseOK int `json:"parse_ok"`

	// Candidates counts parsed candidates (pre-validation).
	Candidates int `json:"candidates"`

	// Valid counts candidates that normalized AND validated.
	Valid int `json:"valid"`

	// Duplicates counts candidates identical to already-known ones.
	Duplicates int `json:"duplicates"`

	// LastLatencyMS is the measured fetch duration of the last
	// SUCCESS (0 = never succeeded).
	LastLatencyMS int64 `json:"last_latency_ms,omitempty"`

	// LastSuccessAt/LastFailureAt are Unix milliseconds (0 = never).
	LastSuccessAt int64 `json:"last_success_at,omitempty"`
	LastFailureAt int64 `json:"last_failure_at,omitempty"`

	// ConsecutiveFailures drives the backoff.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`

	// LastError is the classified last failure (credential free).
	LastError string `json:"last_error,omitempty"`
}

// Availability is the measured connection success fraction.
func (h SourceHealth) Availability() float64 {
	if h.Fetches == 0 {
		return 0 // unknown, never invented
	}

	return float64(h.FetchOK) / float64(h.Fetches)
}

// ParseSuccess is the measured parse success fraction.
func (h SourceHealth) ParseSuccess() float64 {
	if h.FetchOK == 0 {
		return 0
	}

	return float64(h.ParseOK) / float64(h.FetchOK)
}

// ValidYield is the measured fraction of parsed candidates that
// validated.
func (h SourceHealth) ValidYield() float64 {
	if h.Candidates == 0 {
		return 0
	}

	return float64(h.Valid) / float64(h.Candidates)
}

// DuplicateRate is the measured fraction of duplicates among valid
// candidates.
func (h SourceHealth) DuplicateRate() float64 {
	if h.Valid+h.Duplicates == 0 {
		return 0
	}

	return float64(h.Duplicates) / float64(h.Valid+h.Duplicates)
}

// Freshness reports the time since the last successful fetch
// (zero when never succeeded).
func (h SourceHealth) Freshness(now time.Time) time.Duration {
	if h.LastSuccessAt == 0 {
		return 0
	}

	return time.Duration(now.UnixMilli()-h.LastSuccessAt) * time.Millisecond
}

// BackoffUntil returns the time until which the source should be
// skipped (zero = no backoff). Exponential: 2^failures * base,
// capped at maxBackoff — one bad night never becomes a blacklist.
func (h SourceHealth) BackoffUntil() time.Time {
	if h.ConsecutiveFailures == 0 {
		return time.Time{}
	}

	d := backoffBase << uint(clampFailures(h.ConsecutiveFailures))
	if d > maxBackoff {
		d = maxBackoff
	}

	return time.UnixMilli(h.LastFailureAt).Add(d)
}

// InBackoff reports whether the source is currently backing off.
func (h SourceHealth) InBackoff(now time.Time) bool {
	until := h.BackoffUntil()
	return !until.IsZero() && now.Before(until)
}

const (
	backoffBase = 2 * time.Minute
	maxBackoff  = 6 * time.Hour
)

func clampFailures(n int) int {
	if n > 8 {
		return 8
	}

	return n
}

// Health tracks source health for a discovery run. Safe for
// concurrent use.
type Health struct {
	mu      sync.Mutex
	sources map[string]*SourceHealth
	path    string // persistence target ("" = memory only)
}

// NewHealth creates a tracker. When path is non-empty the state is
// loaded (a corrupt file is discarded) and saved on Snapshot.
func NewHealth(path string) *Health {
	h := &Health{
		sources: make(map[string]*SourceHealth),
		path:    path,
	}

	if path != "" {
		h.load()
	}

	return h
}

// RecordFetch observes one fetch attempt.
func (h *Health) RecordFetch(sourceID string, ok bool, latencyMS int64, classifiedErr string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	e := h.entry(sourceID)
	e.Fetches++

	if ok {
		e.FetchOK++
		e.LastLatencyMS = latencyMS
		e.LastSuccessAt = nowMillis()
		e.ConsecutiveFailures = 0
		e.LastError = ""
	} else {
		e.ConsecutiveFailures++
		e.LastFailureAt = nowMillis()
		e.LastError = classifiedErr
	}
}

// RecordParse observes a parse outcome for a fetched body.
func (h *Health) RecordParse(sourceID string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	e := h.entry(sourceID)
	if ok {
		e.ParseOK++
	}
}

// RecordYield observes the candidate yield of one parsed body.
func (h *Health) RecordYield(sourceID string, candidates, valid, duplicates int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	e := h.entry(sourceID)
	e.Candidates += candidates
	e.Valid += valid
	e.Duplicates += duplicates
}

// Get returns a copy of one source's health (zero value when
// unknown).
func (h *Health) Get(sourceID string) SourceHealth {
	h.mu.Lock()
	defer h.mu.Unlock()

	if e, ok := h.sources[sourceID]; ok {
		return *e
	}

	return SourceHealth{SourceID: sourceID}
}

// Rank orders sources by a composite of measured availability, parse
// success, valid yield and freshness. Unknown sources sort neutrally
// in the middle (never punished, never promoted). Sources in backoff
// sink below everything else but keep their position otherwise.
func (h *Health) Rank(now time.Time, sourceIDs []string) []string {
	type scored struct {
		id    string
		score float64
	}

	now = now.UTC()

	entries := make([]scored, 0, len(sourceIDs))

	for _, id := range sourceIDs {
		e := h.Get(id)

		// Composite: connectivity and yield dominate, freshness
		// breaks ties. All factors are measured.
		score := 0.4*e.Availability() + 0.25*e.ParseSuccess() + 0.25*e.ValidYield()

		if e.Fetches == 0 {
			score = 0.3 // unknown: neutral middle
		}

		if age := e.Freshness(now); age > 0 {
			if age < time.Hour {
				score += 0.1
			} else if age > 24*time.Hour {
				score -= 0.1
			}
		}

		if e.InBackoff(now) {
			score = -1 // skip-first, not blacklisted
		}

		entries = append(entries, scored{id: id, score: score})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}

		return entries[i].id < entries[j].id // determinism
	})

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.id)
	}

	return out
}

// Snapshot persists the health state and returns a copy of every
// entry sorted by source ID.
func (h *Health) Snapshot() []SourceHealth {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]SourceHealth, 0, len(h.sources))
	for _, e := range h.sources {
		out = append(out, *e)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].SourceID < out[j].SourceID })

	if h.path != "" {
		_ = h.saveLocked()
	}

	return out
}

// Close persists state (same as Snapshot).
func (h *Health) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.path != "" {
		_ = h.saveLocked()
	}
}

func (h *Health) entry(sourceID string) *SourceHealth {
	e, ok := h.sources[sourceID]
	if !ok {
		e = &SourceHealth{SourceID: sourceID}
		h.sources[sourceID] = e
	}

	return e
}

func (h *Health) load() {
	raw, err := os.ReadFile(h.path)
	if err != nil {
		return // absent: start fresh
	}

	var stored []SourceHealth
	if json.Unmarshal(raw, &stored) != nil {
		return // corrupt: rebuild from observation
	}

	for i := range stored {
		if stored[i].SourceID != "" {
			e := stored[i]
			h.sources[e.SourceID] = &e
		}
	}
}

func (h *Health) saveLocked() error {
	out := make([]SourceHealth, 0, len(h.sources))
	for _, e := range h.sources {
		out = append(out, *e)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].SourceID < out[j].SourceID })

	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return fmt.Errorf("discovery: health mkdir: %w", err)
	}

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("discovery: health encode: %w", err)
	}

	if err := os.WriteFile(h.path, raw, 0o600); err != nil {
		return fmt.Errorf("discovery: health write: %w", err)
	}

	return nil
}

func nowMillis() int64 { return time.Now().UTC().UnixMilli() }
