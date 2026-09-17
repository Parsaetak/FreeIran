package discovery

// Rate-limit engineering for external providers (v0.9.7 §8).
//
// Unauthenticated public GitHub access is limited (60 core requests
// per hour per IP) and GitHub search endpoints have stricter limits
// still; polite public-URL discovery must therefore treat provider
// budgets as a first-class resource:
//
//   - every request is accounted per provider (requests, successes,
//     failures, 429s, 403s, remaining/reset when reported);
//   - a small request budget paces bursts;
//   - concurrency is bounded by a weighted semaphore;
//   - 429/403 responses start a cooldown (Retry-After honoured,
//     exponential otherwise) — never an aggressive retry;
//   - ETag/conditional requests are handled by the caller (the
//     generic fetcher) to keep served bytes low;
//   - rate limiting is NEVER a fatal application error: the engine
//     degrades to cached/local sources and logs a single structured
//     event per provider per episode.

import (
	"errors"
	"sync"
	"time"
)

// Provider identifiers.
const (
	ProviderGitHub = "github"
	ProviderGist   = "github-gist"
	ProviderHTTP   = "http"
)

// ErrProviderCoolingDown reports that the provider is in a rate-limit
// cooldown. Callers must degrade (skip, use cache) rather than retry.
var ErrProviderCoolingDown = errors.New("discovery: provider rate-limit cooldown active")

// ErrBudgetExhausted reports that this discovery run exhausted its
// request budget for the provider.
var ErrBudgetExhausted = errors.New("discovery: provider request budget exhausted")

// ProviderStats is the per-provider accounting snapshot surfaced to
// the UI and the structured log.
type ProviderStats struct {
	Provider       string `json:"provider"`
	Requests       int64  `json:"requests"`
	Successes      int64  `json:"successes"`
	Failures       int64  `json:"failures"`
	RateLimited429 int64  `json:"rate_limited_429"`
	Forbidden403   int64  `json:"forbidden_403"`
	Remaining      int64  `json:"remaining"`          // last reported X-RateLimit-Remaining
	ResetAt        string `json:"reset_at,omitempty"` // last reported reset (RFC3339)
	LastRequestAt  string `json:"last_request_at,omitempty"`
	AvgLatencyMS   int64  `json:"avg_latency_ms"`
	CooldownUntil  string `json:"cooldown_until,omitempty"`
	CoolingDown    bool   `json:"cooling_down"`
}

// providerState is the mutable per-provider record.
type providerState struct {
	mu sync.Mutex

	requests      int64
	successes     int64
	failures      int64
	rateLimited   int64
	forbidden     int64
	remaining     int64
	resetAt       string
	lastRequestAt time.Time
	totalLatency  time.Duration
	latencyCount  int64

	cooldownUntil time.Time
}

// RateLimiter tracks and enforces per-provider request budgets.
type RateLimiter struct {
	mu     sync.Mutex
	states map[string]*providerState

	// budget caps requests per provider per discovery run (0 = 60).
	budget int

	// minInterval paces consecutive requests to one provider (0 = none).
	minInterval time.Duration
}

// RateLimiterOptions configure the limiter.
type RateLimiterOptions struct {
	// Budget caps requests per provider per Run (0 = 60).
	Budget int

	// MinInterval paces consecutive requests to one provider
	// (0 = unpaced; GitHub uses 500ms).
	MinInterval time.Duration
}

// NewRateLimiter creates a limiter.
func NewRateLimiter(opts RateLimiterOptions) *RateLimiter {
	if opts.Budget <= 0 {
		opts.Budget = 60
	}

	return &RateLimiter{
		states:      make(map[string]*providerState),
		budget:      opts.Budget,
		minInterval: opts.MinInterval,
	}
}

// Budget returns the configured per-provider budget.
func (r *RateLimiter) Budget() int {
	if r == nil {
		return 60
	}

	return r.budget
}

// state returns (creating on demand) the state for provider.
func (r *RateLimiter) state(provider string) *providerState {
	r.mu.Lock()
	defer r.mu.Unlock()

	state := r.states[provider]
	if state == nil {
		state = &providerState{}
		r.states[provider] = state
	}

	return state
}

// Acquire accounts one request attempt. It returns ErrBudgetExhausted
// when the per-run budget is spent and ErrProviderCoolingDown during a
// rate-limit cooldown; the caller must degrade instead of retrying.
func (r *RateLimiter) Acquire(provider string) error {
	state := r.state(provider)

	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.cooldownUntil.IsZero() && time.Now().Before(state.cooldownUntil) {
		return ErrProviderCoolingDown
	}

	if state.requests >= int64(r.Budget()) {
		return ErrBudgetExhausted
	}

	if r.minInterval > 0 && !state.lastRequestAt.IsZero() {
		if elapsed := time.Since(state.lastRequestAt); elapsed < r.minInterval {
			// Pace: block briefly rather than reject.
			time.Sleep(r.minInterval - elapsed)
		}
	}

	state.requests++
	state.lastRequestAt = time.Now().UTC()

	return nil
}

// Observe records one request outcome. statusCode 429/403 starts a
// cooldown (retryAfter honoured when provided, exponential otherwise).
func (r *RateLimiter) Observe(provider string, statusCode int, latency time.Duration, retryAfter time.Duration, remaining int64, resetAt string) {
	state := r.state(provider)

	state.mu.Lock()
	defer state.mu.Unlock()

	state.totalLatency += latency
	state.latencyCount++

	if remaining >= 0 {
		state.remaining = remaining
	}

	if resetAt != "" {
		state.resetAt = resetAt
	}

	switch statusCode {
	case -1: // network/transport failure (no HTTP status)
		state.failures++

	case 0: // caller does not track status (success by convention)
		state.successes++

	case 429:
		state.rateLimited++
		state.cooldownUntil = rateLimitCooldown(time.Now(), retryAfter, state.rateLimited)
		state.lastRequestAt = time.Now().UTC()

	case 403:
		state.forbidden++

		// GitHub signals rate limiting with 403 (both when the
		// remaining count hits zero and in many unauth flows). The
		// headers may be lost on the error path (remaining == -1),
		// so every 403 starts a conservative cooldown — graceful
		// degradation beats aggressive retry (§8).
		state.cooldownUntil = rateLimitCooldown(time.Now(), retryAfter, state.forbidden)
		state.lastRequestAt = time.Now().UTC()

	default:
		if statusCode >= 200 && statusCode < 400 {
			state.successes++
		} else {
			state.failures++
		}
	}
}

// rateLimitCooldown computes the cooldown end: server-declared
// Retry-After when present, otherwise exponential from 5 minutes
// doubling per occurrence, capped at 6 hours.
func rateLimitCooldown(now time.Time, retryAfter time.Duration, occurrences int64) time.Time {
	if retryAfter > 0 && retryAfter < 6*time.Hour {
		return now.Add(retryAfter)
	}

	cooldown := 5 * time.Minute << min64(occurrences-1, 7) // 5m,10m,20m,... ≤ 10.7h
	if cooldown > 6*time.Hour {
		cooldown = 6 * time.Hour
	}

	return now.Add(cooldown)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}

	return b
}

// Stats returns a snapshot for one provider (zero-value safe).
func (r *RateLimiter) Stats(provider string) ProviderStats {
	if r == nil {
		return ProviderStats{Provider: provider}
	}

	state := r.state(provider)

	state.mu.Lock()
	defer state.mu.Unlock()

	stats := ProviderStats{
		Provider:       provider,
		Requests:       state.requests,
		Successes:      state.successes,
		Failures:       state.failures,
		RateLimited429: state.rateLimited,
		Forbidden403:   state.forbidden,
		Remaining:      state.remaining,
		ResetAt:        state.resetAt,
	}

	if !state.lastRequestAt.IsZero() {
		stats.LastRequestAt = state.lastRequestAt.Format(time.RFC3339)
	}

	if state.latencyCount > 0 {
		stats.AvgLatencyMS = (state.totalLatency.Milliseconds()) / state.latencyCount
	}

	if !state.cooldownUntil.IsZero() && time.Now().Before(state.cooldownUntil) {
		stats.CoolingDown = true
		stats.CooldownUntil = state.cooldownUntil.Format(time.RFC3339)
	}

	return stats
}

// AllStats returns snapshots for every tracked provider.
func (r *RateLimiter) AllStats() []ProviderStats {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	names := make([]string, 0, len(r.states))
	for name := range r.states {
		names = append(names, name)
	}
	r.mu.Unlock()

	out := make([]ProviderStats, 0, len(names))
	for _, name := range names {
		out = append(out, r.Stats(name))
	}

	return out
}
