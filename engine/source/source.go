package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// bytesSHA256 returns the hex-encoded SHA-256 of data.
func bytesSHA256(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

const (
	DefaultTimeout     = 20 * time.Second
	DefaultMaxBodySize = 10 << 20 // 10 MiB
)

// Source describes a remote configuration source.
//
// In v0.6 the Source model gained a full metadata surface so the UI
// can show reliability, latency and freshness without re-deriving
// them on every render. Metadata fields are persisted in the
// sources.json sidecar and updated by the collector after each
// fetch/parse cycle.
type Source struct {
	// ID is the stable, unique identifier of the source. NEVER
	// renamed (rename = delete + add). Used as the content-hash
	// key in the persisted sources file.
	ID string `json:"id"`

	// Name is the human-friendly display name.
	Name string `json:"name"`

	// URL is the raw fetch URL. MUST be the raw endpoint: GitHub
	// raw.githubusercontent.com, never the /blob/ or /blame/ HTML
	// page.
	URL string `json:"url"`

	// Enabled controls whether the source participates in collection.
	Enabled bool `json:"enabled"`

	// --- v0.6 metadata extension (sources.json v2) ---

	// Provider is the upstream project name (e.g.
	// "ShadowsocksAggregator", "MahsaFreeConfig", "10ium").
	Provider string `json:"provider,omitempty"`

	// Project is the GitHub "owner/repo" identifier when known.
	Project string `json:"project,omitempty"`

	// ProtocolHints lists the protocols the source is expected to
	// contain. Hints speed up parser selection; the parser still
	// accepts any recognized URL scheme.
	ProtocolHints []string `json:"protocol_hints,omitempty"`

	// Region is the geographic or topological region the source
	// targets (e.g. "iran", "netherlands", "global"). Free-form;
	// surfaced as a filter chip in the UI.
	Region string `json:"region,omitempty"`

	// Format is the expected content format: "v2ray-subscription",
	// "clash", "shadowsocks-uri", "mixed", "auto". "auto" runs every
	// parser.
	Format string `json:"format,omitempty"`

	// Priority orders sources within a refresh cycle. Lower = higher
	// priority. Default 100. Sources with the same priority are
	// fetched concurrently.
	Priority int `json:"priority,omitempty"`

	// RefreshInterval overrides the global refresh cadence for one
	// source. Zero = use the global RefreshInterval. Useful for
	// sources that update rarely (e.g. weekly) or very often.
	RefreshInterval time.Duration `json:"refresh_interval,omitempty"`

	// --- Collector-populated statistics (read-only for the UI) ---

	// LastSuccessfulFetch is the most recent time the source was
	// fetched successfully.
	LastSuccessfulFetch time.Time `json:"last_successful_fetch,omitempty"`

	// LastFailure is the most recent time the source failed to
	// fetch or parse.
	LastFailure time.Time `json:"last_failure,omitempty"`

	// LastFailureReason is a short string explaining the last
	// failure (network / parse / 4xx / 5xx / size_limit).
	LastFailureReason string `json:"last_failure_reason,omitempty"`

	// LastContentHash is the SHA-256 of the last successfully
	// fetched body. Used to skip parsing when the body is unchanged.
	LastContentHash string `json:"last_content_hash,omitempty"`

	// ConfigCount is the number of configurations parsed from the
	// source on the last successful cycle.
	ConfigCount int `json:"config_count,omitempty"`

	// WorkingCount is the number of configurations from this source
	// that have a recent successful test result.
	WorkingCount int `json:"working_count,omitempty"`

	// AverageLatencyMS is the rolling mean of recent test latencies
	// for configurations from this source.
	AverageLatencyMS int64 `json:"average_latency_ms,omitempty"`

	// ReliabilityScore is a 0-100 score derived from
	// (successful_fetches / total_fetches) * 100 over a rolling
	// window. Updated by the collector.
	ReliabilityScore int `json:"reliability_score,omitempty"`

	// FetchCount is the total number of fetch attempts.
	FetchCount int `json:"fetch_count,omitempty"`

	// SuccessCount is the number of successful fetches.
	SuccessCount int `json:"success_count,omitempty"`

	// ETag is the HTTP ETag header from the last successful fetch,
	// used for conditional requests (If-None-Match).
	ETag string `json:"etag,omitempty"`

	// LastModified is the HTTP Last-Modified header from the last
	// successful fetch, used for conditional requests
	// (If-Modified-Since).
	LastModifiedHeader string `json:"last_modified_header,omitempty"`

	// Custom marks user-added sources (not in the default list).
	// Custom sources are never auto-removed by the registry update.
	Custom bool `json:"custom,omitempty"`
}

// Stats is the runtime statistics block for one source, returned to
// the UI in the source registry snapshot. It is derived from the
// Source metadata fields above; keeping it as a separate struct lets
// the UI bind a stable shape even when Source grows.
type Stats struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	URL                 string    `json:"url"`
	Provider            string    `json:"provider"`
	Region              string    `json:"region"`
	Enabled             bool      `json:"enabled"`
	Priority            int       `json:"priority"`
	LastSuccessfulFetch time.Time `json:"last_successful_fetch"`
	LastFailure         time.Time `json:"last_failure,omitempty"`
	LastFailureReason   string    `json:"last_failure_reason,omitempty"`
	ConfigCount         int       `json:"config_count"`
	WorkingCount        int       `json:"working_count"`
	AverageLatencyMS    int64     `json:"average_latency_ms"`
	ReliabilityScore    int       `json:"reliability_score"`
	FetchCount          int       `json:"fetch_count"`
	SuccessCount        int       `json:"success_count"`
}

// Stats returns the public statistics view of a source.
func (s Source) Stats() Stats {
	priority := s.Priority
	if priority == 0 {
		priority = 100
	}
	return Stats{
		ID:                  s.ID,
		Name:                s.Name,
		URL:                 s.URL,
		Provider:            s.Provider,
		Region:              s.Region,
		Enabled:             s.Enabled,
		Priority:            priority,
		LastSuccessfulFetch: s.LastSuccessfulFetch,
		LastFailure:         s.LastFailure,
		LastFailureReason:   s.LastFailureReason,
		ConfigCount:         s.ConfigCount,
		WorkingCount:        s.WorkingCount,
		AverageLatencyMS:    s.AverageLatencyMS,
		ReliabilityScore:    s.ReliabilityScore,
		FetchCount:          s.FetchCount,
		SuccessCount:        s.SuccessCount,
	}
}

// Result contains the downloaded source content and metadata.
//
// In v0.6 the Result gained NotModified (true when the server returned
// 304 Not Modified for a conditional request) and ContentHash (the
// SHA-256 of Content, computed unconditionally so callers can dedup
// without re-hashing).
type Result struct {
	Source      Source
	Content     []byte
	FetchedAt   time.Time
	StatusCode  int
	ContentType string

	// NotModified is true when the server returned 304 Not Modified.
	// In that case Content is empty; the caller should reuse the
	// previous body (no parse needed).
	NotModified bool `json:"not_modified"`

	// ContentHash is the hex SHA-256 of Content (empty when
	// NotModified).
	ContentHash string `json:"content_hash,omitempty"`

	// ETag / LastModified are echoed from the response so the
	// caller can persist them for the next conditional request.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

// Fetcher downloads configuration sources through the ONE shared
// production HTTP policy engine (internal/httpx): retries with
// exponential backoff + jitter for transient failures and 429/502/
// 503/504, Retry-After honouring, connection reuse and bounded
// response bodies.
//
// Per-source isolation (one broken source must not break the others):
//
//   - a bounded per-source budget (Timeout, default 20s) that covers
//     the request AND its retries, enforced through the context;
//   - a response-size hard cap (default 10 MiB);
//   - conditional requests: If-None-Match (ETag) and
//     If-Modified-Since (Last-Modified) from the Source metadata;
//   - content-hash short-circuit: if the response body hashes to the
//     same value as Source.LastContentHash, the parser is told the
//     body is unchanged (NotModified=true).
//
// Parse isolation is provided by the ingestion pipeline, which recovers
// per-source parser panics so one hostile payload cannot kill the
// cycle.
type Fetcher struct {
	// Client is the shared production HTTP client. nil = httpx.Default.
	Client httpx.Getter

	// Timeout bounds one source fetch INCLUDING retries (per-source
	// isolation). Zero = DefaultTimeout.
	Timeout time.Duration

	// MaxBodySize caps the response body. Zero = DefaultMaxBodySize.
	MaxBodySize int64
}

// NewFetcher creates a Fetcher with safe defaults.
func NewFetcher() *Fetcher {
	return &Fetcher{
		Client:      httpx.Default(),
		Timeout:     DefaultTimeout,
		MaxBodySize: DefaultMaxBodySize,
	}
}

// Fetch downloads one source. The fetcher transparently:
//
//   - adds gzip/deflate to Accept-Encoding (net/http handles
//     decompression automatically when Transport.DisableCompression
//     is false, which is the default);
//   - sends If-None-Match (ETag) and If-Modified-Since (Last-Modified)
//     when the Source carries them from a previous cycle, so the
//     server can return 304 Not Modified and skip the body entirely;
//   - computes the SHA-256 of the body and compares it to
//     Source.LastContentHash: if they match the body is marked
//     NotModified even when the server returned 200 (some CDNs do
//     not implement conditional requests correctly).
//   - echoes the response ETag / Last-Modified so the caller can
//     persist them for the next cycle.
func (f *Fetcher) Fetch(ctx context.Context, src Source) (Result, error) {
	if strings.TrimSpace(src.URL) == "" {
		return Result{}, fmt.Errorf("source URL is empty")
	}

	if !src.Enabled {
		return Result{}, fmt.Errorf("source %q is disabled", src.ID)
	}

	client := f.Client
	if client == nil {
		client = httpx.Default()
	}

	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	maxBodySize := f.MaxBodySize
	if maxBodySize <= 0 {
		maxBodySize = DefaultMaxBodySize
	}

	// Per-source isolation: the bounded budget (including retries)
	// applies to THIS source only; the caller's ctx still governs the
	// overall cycle.
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Conditional requests: send the ETag from the previous successful
	// fetch. If the body has not changed, the server returns 304.
	header := map[string]string{
		"Accept": "*/*",
	}

	if src.LastModifiedHeader != "" {
		header["If-Modified-Since"] = src.LastModifiedHeader
	}

	resp, err := client.Get(fetchCtx, src.URL, httpx.GetOptions{
		Header:       header,
		IfNoneMatch:  src.ETag,
		MaxBodyBytes: maxBodySize,
	})
	if err != nil {
		return Result{}, fmt.Errorf("fetch source %q: %w", src.ID, err)
	}

	if resp.StatusCode == http.StatusNotModified {
		// Server confirmed the body is unchanged. No parse needed.
		return Result{
			Source:       src,
			Content:      nil,
			FetchedAt:    time.Now().UTC(),
			StatusCode:   resp.StatusCode,
			ContentType:  resp.Header.Get("Content-Type"),
			NotModified:  true,
			ETag:         resp.Header.Get("ETag"),
			LastModified: resp.Header.Get("Last-Modified"),
		}, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf(
			"source %q returned HTTP %d",
			src.ID,
			resp.StatusCode,
		)
	}

	// The shared policy already bounded the body to maxBodySize; the
	// local length check documents the invariant for callers.
	content := resp.Body

	if int64(len(content)) > maxBodySize {
		return Result{}, fmt.Errorf(
			"source %q exceeds maximum size of %d bytes",
			src.ID,
			maxBodySize,
		)
	}

	hash := bytesSHA256(content)

	// Content-hash short-circuit: even when the server returned 200,
	// if the body hashes to the same value as the previous cycle,
	// the body has not changed. The parser can skip work.
	notModified := src.LastContentHash != "" && hash == src.LastContentHash

	return Result{
		Source:       src,
		Content:      content,
		FetchedAt:    time.Now().UTC(),
		StatusCode:   resp.StatusCode,
		ContentType:  resp.Header.Get("Content-Type"),
		NotModified:  notModified,
		ContentHash:  hash,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}, nil
}
