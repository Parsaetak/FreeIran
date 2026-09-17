package config

import "time"

// metrics.go defines the v0.9.6 measurement model: the structured
// results of the two first-class test modes (Ping and URL) plus the
// protocol-handshake timing. These types are the shared vocabulary of
// the whole pipeline — the tester produces them, the store persists
// them inside Config records, the ranking layer scores from them and
// the UI renders them. Everything is measured, never estimated: a
// missing measurement is simply nil/zero and is displayed as
// "unavailable", never as a fabricated number.

// PingMetrics is the outcome of a repeated-sample latency measurement
// against a candidate's endpoint (TCP handshake RTT — see
// engine/tester/ping.go for why FreeIran measures TCP, not ICMP).
//
// Every field is derived from the recorded samples; there is no
// interpolation and no invented value. Zero PingMetrics (Samples=0)
// means "not measured".
type PingMetrics struct {
	// Min/Max are the fastest and slowest successful samples.
	MinMS int64 `json:"min_ms"`
	MaxMS int64 `json:"max_ms"`

	// Median is the 50th percentile of the successful samples
	// (resistant to outliers, the number the "Lowest Median Ping"
	// sort uses).
	MedianMS int64 `json:"median_ms"`

	// Avg is the arithmetic mean of the successful samples.
	AvgMS int64 `json:"avg_ms"`

	// Jitter is the mean absolute deviation between consecutive
	// successful samples (instability of the path, in ms).
	JitterMS int64 `json:"jitter_ms"`

	// PacketLoss is the fraction of samples that failed (timeouts +
	// refused + reset) in [0,1].
	PacketLoss float64 `json:"packet_loss"`

	// Samples counts the successful round trips; Failures counts
	// every failed attempt; Timeouts counts the subset of failures
	// that were deadline exceeded (path black-holing).
	Samples  int `json:"samples"`
	Failures int `json:"failures"`
	Timeouts int `json:"timeouts"`

	// At is the measurement time (Unix milliseconds). Staleness is
	// judged against this timestamp, never against discovery time.
	At int64 `json:"at"`
}

// Age reports how old the measurement is relative to now.
func (p *PingMetrics) Age(now time.Time) time.Duration {
	if p == nil || p.At == 0 {
		return 0
	}

	return time.Duration(now.UnixMilli()-p.At) * time.Millisecond
}

// URLTestMetrics is the outcome of one HTTP connectivity measurement
// THROUGH the candidate tunnel — proving usable connectivity, not
// merely that a host responds.
//
// Phase timings are the real httptrace phase durations in
// milliseconds; -1 marks a phase that did not complete (e.g. TLS
// timing on a plaintext request) and 0 is a valid, very fast phase.
type URLTestMetrics struct {
	// URL is the test target that was requested (recorded so results
	// are comparable across configuration changes).
	URL string `json:"url,omitempty"`

	// DNSMS is the name-resolution phase (0 when the target was an
	// IP literal or the proxy resolved it remotely).
	DNSMS int64 `json:"dns_ms,omitempty"`

	// ConnectMS is the TCP connection phase.
	ConnectMS int64 `json:"connect_ms,omitempty"`

	// TLSMS is the TLS handshake phase (-1 when no TLS was used).
	TLSMS int64 `json:"tls_ms,omitempty"`

	// TTFBMS is time-to-first-byte from the request write to the
	// first response byte.
	TTFBMS int64 `json:"ttfb_ms,omitempty"`

	// TotalMS is the complete request wall time.
	TotalMS int64 `json:"total_ms"`

	// Status is the HTTP status code (0 when the request never
	// completed).
	Status int `json:"status"`

	// Bytes is the response body size actually received.
	Bytes int64 `json:"bytes,omitempty"`

	// OK reports usable connectivity: the request completed with a
	// 2xx/3xx status within the configured timeout.
	OK bool `json:"ok"`

	// Timeout marks a failure caused by the configured deadline
	// (versus refused/reset/protocol errors).
	Timeout bool `json:"timeout,omitempty"`

	// Error is the failure classification (empty on success); it is
	// derived from the transport error, never raw credentials.
	Error string `json:"error,omitempty"`

	// At is the measurement time (Unix milliseconds).
	At int64 `json:"at"`
}

// Age reports how old the measurement is relative to now.
func (u *URLTestMetrics) Age(now time.Time) time.Duration {
	if u == nil || u.At == 0 {
		return 0
	}

	return time.Duration(now.UnixMilli()-u.At) * time.Millisecond
}

// HandshakeMetrics records the protocol-core handshake phase of a
// test (core start → inbound ready), the v0.9.3 DurationMS split
// into its meaningful part.
type HandshakeMetrics struct {
	// ReadyMS is the time from core process start to the inbound
	// listener accepting connections.
	ReadyMS int64 `json:"ready_ms,omitempty"`

	// ProbeMS is the end-to-end probe time through the ready tunnel
	// (when the probe ran).
	ProbeMS int64 `json:"probe_ms,omitempty"`

	// OK reports whether the handshake completed.
	OK bool `json:"ok"`

	// At is the measurement time (Unix milliseconds).
	At int64 `json:"at"`
}
