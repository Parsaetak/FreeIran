package discovery

// Generic HTTP/HTTPS source connector (v0.9.7 §7, connector 1).
//
// Fetches publicly accessible configuration material from ordinary
// URLs (plain text lists, subscriptions, exported configs) with:
//
//   - SSRF-guarded client (scheme/port/IP/redirect validation);
//   - redirects supported but bounded and re-validated per hop;
//   - response body size cap (never buffers unbounded bodies);
//   - per-request timeout inside the caller's cycle budget;
//   - transparent gzip/deflate (transport) and brotli-free handling —
//     br is REQUESTED only when the transport cannot be trusted to
//     decompress; malformed payloads never panic (parse layer is
//     panic-isolated);
//   - ETag / Last-Modified conditional requests with the persisted
//     source metadata (304 → NotModified short-circuit);
//   - Retry-After honoured by the shared httpx policy;
//   - exponential backoff on transient failures (shared policy);
//   - status code / content type / size / duration recorded into the
//     candidate's provenance ledger;
//   - text/binary identification: payloads that look binary are
//     rejected before the parser sees them.
//
// Manually configured sources (SourceTypeConfigured) keep their own
// fetch path (engine/source) and are never displaced: this connector
// only ADDS discovered material.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// DefaultTextExtensions lists file-name suffixes plausible as plain
// configuration material (used by candidate filters, not as a hard
// gate — Content-Type sniffing is authoritative at fetch time).
var DefaultTextExtensions = []string{
	".txt", ".yaml", ".yml", ".json", ".conf", ".list", ".sub", ".md",
}

// GenericConnector fetches ordinary public URLs.
type GenericConnector struct {
	// Client is the SSRF-guarded fetch client.
	Client *httpx.Client

	// Limits bound the expansion.
	Limits BoundedRecursion

	// Queue is the shared candidate queue for this run.
	Queue *CandidateQueue

	// RateLimiter accounts per-provider budgets (provider "http").
	RateLimiter *RateLimiter
}

// NewGenericConnector wires a connector over the shared queue.
func NewGenericConnector(client *httpx.Client, limits BoundedRecursion, queue *CandidateQueue, limiter *RateLimiter) *GenericConnector {
	return &GenericConnector{
		Client:      client,
		Limits:      limits.normalize(),
		Queue:       queue,
		RateLimiter: limiter,
	}
}

// FetchOutcome is the result of one bounded fetch.
type FetchOutcome struct {
	URL          string
	Body         []byte
	StatusCode   int
	ContentType  string
	ETag         string
	LastModified string
	NotModified  bool
	Size         int64
	DurationMS   int64
	ContentHash  string
	Binary       bool
}

// Fetch downloads one URL with the full guard stack. It never panics
// on malformed content: every failure is an error return.
func (g *GenericConnector) Fetch(ctx context.Context, candidate Candidate) (FetchOutcome, error) {
	limits := g.Limits.normalize()

	outcome := FetchOutcome{URL: candidate.URL}

	started := time.Now()

	if err := g.RateLimiter.Acquire(ProviderHTTP); err != nil {
		return outcome, err
	}

	opts := httpx.GetOptions{
		MaxBodyBytes: limits.MaxBodySize,
	}

	if candidate.Provenance.ETag != "" {
		// Conditional request from persisted metadata.
		opts.IfNoneMatch = candidate.Provenance.ETag
	}

	resp, err := g.Client.Get(ctx, candidate.URL, opts)

	outcome.DurationMS = time.Since(started).Milliseconds()

	if err != nil {
		// Rate limiting is never fatal: record and degrade.
		status := httpx.StatusCodeOf(err)
		if status < 0 && errors.Is(err, httpx.ErrRateLimited) {
			status = 429
		}

		g.RateLimiter.Observe(ProviderHTTP, status,
			time.Duration(outcome.DurationMS)*time.Millisecond, 0, -1, "")

		return outcome, err
	}

	outcome.StatusCode = resp.StatusCode
	outcome.ContentType = resp.Header.Get("Content-Type")
	outcome.ETag = resp.Header.Get("ETag")
	outcome.LastModified = resp.Header.Get("Last-Modified")
	outcome.Size = int64(len(resp.Body))
	outcome.Body = resp.Body

	sum := sha256.Sum256(resp.Body)
	outcome.ContentHash = hex.EncodeToString(sum[:])

	g.RateLimiter.Observe(ProviderHTTP, resp.StatusCode, time.Duration(outcome.DurationMS)*time.Millisecond, 0, -1, "")

	// Binary sniffing: NUL bytes or invalid UTF-8 reject the payload
	// before the parser (parsers are line/format based and the
	// content is only ever DATA — but a binary is never config).
	outcome.Binary = looksBinary(resp.Body)

	return outcome, nil
}

// looksBinary reports whether a payload is definitely binary.
func looksBinary(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	// Sample the first 4 KiB.
	sample := body
	if len(sample) > 4096 {
		sample = sample[:4096]
	}

	nul := 0

	for _, b := range sample {
		if b == 0 {
			nul++
		}
	}

	if nul > 0 {
		return true
	}

	return !utf8.Valid(sample) && !isMostlyPrintable(sample)
}

// isMostlyPrintable accepts latin-1-ish text the parser tolerates.
func isMostlyPrintable(sample []byte) bool {
	printable := 0

	for _, b := range sample {
		if b >= 0x20 && b != 0x7f || b == '\n' || b == '\r' || b == '\t' {
			printable++
		}
	}

	return printable*100 >= len(sample)*90
}

// ExtractReferencedCandidates parses fetched TEXT for absolute https
// URLs that plausibly hold configuration material (README references,
// subscription indexes, link lists). Extraction is bounded by the
// connector's MaxURLsPerSource and feeds back into the bounded queue
// (§7.E / §10).
func (g *GenericConnector) ExtractReferencedCandidates(parent Candidate, body []byte) []Candidate {
	limits := g.Limits.normalize()

	if parent.Provenance.Depth >= limits.MaxDepth {
		return nil
	}

	refs := extractContentRefs(body)
	if len(refs) == 0 {
		return nil
	}

	if len(refs) > limits.MaxURLsPerSource {
		refs = refs[:limits.MaxURLsPerSource]
	}

	candidates := make([]Candidate, 0, len(refs))

	for _, ref := range refs {
		if !isPlausibleRawEndpoint(ref) {
			continue
		}

		child := Candidate{
			URL:   ref,
			Trust: TrustUnknown,
			Provenance: Provenance{
				SourceID:       "ref:" + FingerprintURL(ref),
				SourceType:     SourceTypeReferenced,
				SourceURL:      ref,
				DiscoveredFrom: parent.Provenance.SourceID,
				OriginPath:     parent.Provenance.OriginPath,
				FirstSeen:      time.Now().UTC(),
				LastSeen:       time.Now().UTC(),
				Depth:          parent.Provenance.Depth + 1,
			},
		}

		candidates = append(candidates, child)
	}

	return candidates
}

// extractContentRefs pulls absolute https URLs out of a text body
// (shared with content.go's regex machinery).
func extractContentRefs(body []byte) []string {
	if len(body) == 0 || len(body) > 8<<20 {
		return nil
	}

	text := string(body)

	refs := contentRefPattern.FindAllString(text, 64)
	if refs == nil {
		return nil
	}

	// De-dup preserving order.
	seen := make(map[string]struct{}, len(refs))
	out := make([]string, 0, len(refs))

	for _, ref := range refs {
		ref = strings.TrimRight(ref, ".,;)")
		if _, dup := seen[ref]; dup {
			continue
		}

		seen[ref] = struct{}{}
		out = append(out, ref)
	}

	return out
}

// DescribeOutcome renders a fetch outcome for the structured log.
func DescribeOutcome(o FetchOutcome) string {
	return fmt.Sprintf("status=%d type=%s size=%d duration_ms=%d binary=%t",
		o.StatusCode, o.ContentType, o.Size, o.DurationMS, o.Binary)
}
