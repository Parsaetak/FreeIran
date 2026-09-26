package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Type identifies the protocol used by a configuration.
type Type string

const (
	TypeVLESS       Type = "vless"
	TypeVMess       Type = "vmess"
	TypeTrojan      Type = "trojan"
	TypeShadowsocks Type = "shadowsocks"
	TypeHysteria    Type = "hysteria"
	TypeHysteria2   Type = "hysteria2"
	TypeTUIC        Type = "tuic"
	TypeWireGuard   Type = "wireguard"
	TypeSOCKS       Type = "socks"
	TypeHTTP        Type = "http"
	TypeUnknown     Type = "unknown"
)

// v0.10.4 value domains for enumerated protocol details. These are
// the ONLY values the parsers may produce and the backends generate;
// validation rejects anything else instead of forwarding garbage to
// the core at runtime.
const (
	// ObfsSalamander and ObfsGecko are the obfuscation protocols
	// sing-box v1.14 implements for the Hysteria2 outbound
	// (`obfs.type`). The Hysteria (v1) outbound has DIFFERENT
	// semantics: its `obfs` is a plain JSON string carrying the
	// obfuscation password itself — never a type name (see
	// docs/protocols.md and the v0.10.4 changelog).
	ObfsSalamander = "salamander"
	ObfsGecko      = "gecko"

	// TUIC congestion-control algorithms (sing-box v1.14 tuic
	// outbound `congestion_control`).
	CongestionControlBBR     = "bbr"
	CongestionControlCubic   = "cubic"
	CongestionControlNewReno = "new_reno"

	// TUIC UDP relay modes (sing-box v1.14 tuic outbound
	// `udp_relay_mode`). v0.10.4 correction: the v0.10.3 enum said
	// "quadratic", which no sing-box release documents — the
	// actual domain is native | quic.
	UDPRelayModeNative = "native"
	UDPRelayModeQUIC   = "quic"
)

// TUICCongestionControlValues is the exhaustive value domain.
var TUICCongestionControlValues = []string{
	CongestionControlBBR,
	CongestionControlCubic,
	CongestionControlNewReno,
}

// TUICUDPRelayModeValues is the exhaustive value domain.
var TUICUDPRelayModeValues = []string{
	UDPRelayModeNative,
	UDPRelayModeQUIC,
}

// Hysteria2ObfsValues is the exhaustive obfs.type domain of the
// Hysteria2 outbound (sing-box v1.14). Hysteria (v1) does not share
// this domain — its obfs is a password string, not an enum.
var Hysteria2ObfsValues = []string{
	ObfsSalamander,
	ObfsGecko,
}

// ValidHysteria2Obfs reports whether v is an accepted Hysteria2
// obfs type. The empty value means "no obfuscation" and is handled
// by the callers.
func ValidHysteria2Obfs(v string) bool {
	for _, ok := range Hysteria2ObfsValues {
		if v == ok {
			return true
		}
	}

	return false
}

// ValidTUICCongestionControl reports whether v is an accepted
// congestion_control value.
func ValidTUICCongestionControl(v string) bool {
	for _, ok := range TUICCongestionControlValues {
		if v == ok {
			return true
		}
	}

	return false
}

// ValidTUICUDPRelayMode reports whether v is an accepted
// udp_relay_mode value.
func ValidTUICUDPRelayMode(v string) bool {
	for _, ok := range TUICUDPRelayModeValues {
		if v == ok {
			return true
		}
	}

	return false
}

// Config is the normalized representation of a VPN/proxy configuration.
//
// Parsers convert external formats into Config objects.
// Protocol engines later convert Config objects into core-specific
// configurations.
// Source-trust bands (v0.9.8.6) — mirror engine/source.Trust. Kept
// as plain string constants so the persisted record stays
// self-describing without importing the source package.
const (
	// SourceTrustOfficial: first-party FreeIran sources.
	SourceTrustOfficial = "official"
	// SourceTrustUser: user-configured sources.
	SourceTrustUser = "user"
	// SourceTrustPublic: third-party public sources — untrusted
	// routes. The default for empty (legacy) records.
	SourceTrustPublic = "public"
)

// RouteTrusted reports whether a configuration's source is trusted
// for AUTOMATIC route selection (official or user). Public/empty
// bands require explicit user choice or opt-in.
func (c Config) RouteTrusted() bool {
	switch c.SourceTrust {
	case SourceTrustOfficial, SourceTrustUser:
		return true
	default:
		return false
	}
}

type Config struct {
	ID      string `json:"id"`
	Type    Type   `json:"type"`
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
	Port    int    `json:"port"`

	// Authentication / identity.
	UUID     string `json:"uuid,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Method   string `json:"method,omitempty"`

	// Transport.
	Network string `json:"network,omitempty"`
	Path    string `json:"path,omitempty"`
	Host    string `json:"host,omitempty"`
	Service string `json:"service,omitempty"`

	// Security.
	Security           string `json:"security,omitempty"`
	ServerName         string `json:"server_name,omitempty"`
	FingerprintProfile string `json:"fingerprint,omitempty"`
	PublicKey          string `json:"public_key,omitempty"`
	ShortID            string `json:"short_id,omitempty"`

	// Insecure is the user-supplied "skip TLS verification" request
	// (v0.10.2: captured for the QUIC family — hysteria/hysteria2/
	// tuic — whose URI spec carries insecure=1; v0.10.1 silently
	// dropped it, making most real-world Hysteria2 configs
	// unusable). Deliberately NOT part of the fingerprint: it
	// changes TLS behavior, not the configuration identity.
	Insecure bool `json:"insecure,omitempty"`

	// Protocol details consumed by core backends. Parsers capture them
	// and backend adapters read them, but they are deliberately NOT part
	// of the fingerprint: identity is endpoint + credentials + transport,
	// so existing stored records keep stable keys across the v0.4
	// protocol-core upgrade (fingerprint stability is a
	// storage-compatibility guarantee, see docs/storage-format.md).
	Flow       string   `json:"flow,omitempty"`        // VLESS flow (xtls-rprx-vision).
	Encryption string   `json:"encryption,omitempty"`  // VLESS encryption (default none).
	AlterID    int      `json:"alter_id,omitempty"`    // VMess legacy alterId.
	HeaderType string   `json:"header_type,omitempty"` // VMess TCP header obfuscation.
	ALPN       []string `json:"alpn,omitempty"`        // TLS ALPN list.
	SpiderX    string   `json:"spider_x,omitempty"`    // REALITY spider path.

	// v0.10.3 protocol-detail fields. v0.10.2 overloaded unrelated
	// slots (TUIC congestion_control → Network; Hysteria obfs →
	// Security; obfs-password → Host), which corrupted BOTH meanings:
	// the capability matcher treated "bbr" as a transport, and the
	// WS host header slot carried an obfuscation password. Each
	// value now lives in its semantic field and generates the
	// correct sing-box JSON. Not part of the fingerprint (tuning,
	// not identity).
	// v0.10.4 semantics note: Obfs means different things for the
	// two Hysteria protocols, matching each actual wire format:
	//   - Hysteria2: the obfs TYPE ("" | salamander | gecko) and
	//     ObfsPassword carries the obfuscation password — generated
	//     as the sing-box object {"type", "password"}.
	//   - Hysteria (v1): the obfuscation PASSWORD string itself
	//     (the v1 format has no type concept) — generated as the
	//     sing-box JSON string `"obfs": "..."`, omitted when empty.
	CongestionControl string `json:"congestion_control,omitempty"` // TUIC: bbr | cubic | new_reno.
	UDPRelayMode      string `json:"udp_relay_mode,omitempty"`     // TUIC: native | quic.
	Obfs              string `json:"obfs,omitempty"`               // Hysteria2: salamander | gecko; Hysteria v1: obfs password string.
	ObfsPassword      string `json:"obfs_password,omitempty"`      // Hysteria2 obfs password (object shape).

	// v0.11.0 Encrypted Client Hello (TLS-layer detail, sing-box
	// tls.ech — verified against the pinned v1.14.0 binary).
	//
	// Semantic model — one dedicated field per sing-box knob,
	// overloading nothing (Host/Network/Security/Fingerprint/
	// SpiderX stay untouched):
	//   ECHEnabled         → ech.enabled
	//   ECHConfig          → ech.config        (PEM "ECH CONFIGS")
	//   ECHConfigPath      → ech.config_path   (file containing the PEM)
	//   ECHQueryServerName → ech.query_server_name (DNS HTTPS query)
	//
	// ECHConfig carries the canonical PEM form after Normalize:
	// a raw base64 ECHConfigList (the DNS HTTPS record `ech=`
	// payload shape) is wrapped into the PEM envelope during
	// normalization, because the pinned sing-box v1.14.0 accepts
	// ONLY the PEM form ("invalid ECH configs pem" otherwise —
	// verified empirically, see docs/protocols.md). Not part of
	// the fingerprint: ECH changes the TLS layer's negotiation,
	// not the configuration identity (same rationale as ALPN and
	// Insecure).
	//
	// EVIDENCE SCOPE (do not upgrade): support is schema-level —
	// `sing-box check` + startup acceptance. No live ECH
	// negotiation with a real ECH server is claimed anywhere.
	ECHEnabled         bool   `json:"ech_enabled,omitempty"`
	ECHConfig          string `json:"ech_config,omitempty"`
	ECHConfigPath      string `json:"ech_config_path,omitempty"`
	ECHQueryServerName string `json:"ech_query_server_name,omitempty"`

	// WireGuard.
	//
	// v0.10.3 semantics split: InterfaceAddress is the LOCAL
	// interface address list (the WireGuard INI "Address" — what the
	// tun interface gets); AllowedIPs is the PEER routing list (what
	// traffic the peer accepts). They are distinct concepts and must
	// never overload each other: v0.10.2 dropped the interface
	// address entirely and generated a WireGuard endpoint with no
	// local address at all.
	PrivateKey          string   `json:"private_key,omitempty"`
	InterfaceAddress    []string `json:"interface_address,omitempty"`
	AllowedIPs          []string `json:"allowed_ips,omitempty"`
	DNS                 []string `json:"dns,omitempty"`
	MTU                 int      `json:"mtu,omitempty"`
	PersistentKeepalive int      `json:"persistent_keepalive,omitempty"`

	// Bandwidth caps for the QUIC family (v0.10.2). Hysteria (v1)
	// REQUIRES both — the pinned sing-box v1.14.0 refuses the
	// outbound without them ("missing upload speed") — while
	// Hysteria2 treats them as optional. Not part of the
	// fingerprint (link tuning, not identity).
	UpMbps   int `json:"up_mbps,omitempty"`
	DownMbps int `json:"down_mbps,omitempty"`

	// Source information.
	Source string `json:"source,omitempty"`

	// SourceTrust is the ROUTE-trust classification of the source this
	// configuration was ingested from (v0.9.8.6):
	// "official" | "user" | "public". Empty (legacy records) resolves
	// as "public" — untrusted by default. Reliability and reachability
	// measurements never promote a public route to trusted.
	SourceTrust string `json:"source_trust,omitempty"`

	// Runtime information.
	Working   bool  `json:"working"`
	LatencyMS int64 `json:"latency_ms,omitempty"`
	TestedAt  int64 `json:"tested_at,omitempty"`

	// v0.9.0 test metadata (§4): the backend that executed the last
	// test, the endpoint it targeted and the total test duration.
	// Runtime-only, never part of the fingerprint.
	TestBackend    string `json:"test_backend,omitempty"`
	TestEndpoint   string `json:"test_endpoint,omitempty"`
	TestDurationMS int64  `json:"test_duration_ms,omitempty"`

	// v0.9.3 bounded observation history: the last TestHistoryLimit
	// test outcomes for this configuration (newest last). It is the
	// actual data the ranking engine scores — success rate, latency
	// stability and timeout frequency come from here, never invented.
	// Runtime-only: never part of the fingerprint, never uploaded.
	TestHistory []TestObservation `json:"test_history,omitempty"`

	// --- v0.9.6 first-class test modes (§8) ---

	// Ping holds the latest repeated-sample TCP latency measurement
	// (min/median/avg/max/jitter/loss). Nil = never ping-tested.
	// Runtime-only: never part of the fingerprint.
	Ping *PingMetrics `json:"ping,omitempty"`

	// URLTest holds the latest HTTP connectivity measurement through
	// the candidate tunnel with its full phase breakdown (DNS/TCP/
	// TLS/TTFB/total). Nil = never URL-tested. Runtime-only: never
	// part of the fingerprint.
	URLTest *URLTestMetrics `json:"url_test,omitempty"`

	// Handshake holds the latest protocol-core handshake timing.
	// Runtime-only: never part of the fingerprint.
	Handshake *HandshakeMetrics `json:"handshake,omitempty"`

	// LastSuccessAt is the last time this candidate provided verified
	// usable connectivity (Unix milliseconds) — the freshness anchor
	// for "Recently Verified" ranking. Runtime-only.
	LastSuccessAt int64 `json:"last_success_at,omitempty"`

	// FailureStreak counts consecutive failed verifications since the
	// last success; reset on every success. Runtime-only.
	FailureStreak int `json:"failure_streak,omitempty"`

	// LastFailureReason is the classified cause of the most recent
	// failure (timeout/refused/reset/handshake/verify), stored
	// without credentials. Runtime-only.
	LastFailureReason string `json:"last_failure_reason,omitempty"`

	// v0.11.0 failure-evidence model (bounded, derived — never a
	// magical "confidence score"):

	// LastFailureAt is the time of the most recent FAILED
	// verification (Unix milliseconds; 0 = no recorded failure).
	// Together with LastSuccessAt it gives the ranking layer an
	// honest verification age on both sides (fresh success vs
	// fresh failure). Runtime-only.
	LastFailureAt int64 `json:"last_failure_at,omitempty"`

	// LastFailureClass is the FAILURE CLASS of the most recent
	// failure — one of the FailureClass* constants (dns, tcp,
	// tls, handshake, listener, verify, reset, timeout,
	// transport). Unlike LastFailureReason (free text), the class
	// is a stable vocabulary the ranking layer and recovery
	// pipeline can reason over ("after two handshake-class
	// failures prefer a different transport"). Runtime-only.
	LastFailureClass string `json:"last_failure_class,omitempty"`
}

// Failure-class vocabulary (v0.11.0). The classes are derived from
// actual observations — the transport error text, the URL-test phase
// that failed and the supervision events — never invented. They are
// deliberately coarse: fine-grained taxonomies drift as cores change
// their error strings, while these nine classes map onto distinct
// ENGINE behaviors (retry, switch transport, wait, or surface).
const (
	// FailureClassDNS: name resolution failed (no such host,
	// servfail, resolver unreachable).
	FailureClassDNS = "dns"
	// FailureClassTCP: TCP connection failed before TLS (refused,
	// unreachable, no route).
	FailureClassTCP = "tcp"
	// FailureClassTLS: TLS negotiation failed (certificate,
	// alert, version, ECH/REALITY layer).
	FailureClassTLS = "tls"
	// FailureClassHandshake: the protocol-core handshake failed
	// (inbound never became ready, core exited during startup).
	FailureClassHandshake = "handshake"
	// FailureClassListener: the local inbound listener could not
	// be established (port in use, bind failure).
	FailureClassListener = "listener"
	// FailureClassVerify: the tunnel came up but external
	// verification failed (through-tunnel fetch rejected).
	FailureClassVerify = "verify"
	// FailureClassReset: the remote side reset an established
	// connection mid-stream.
	FailureClassReset = "reset"
	// FailureClassTimeout: a bounded operation exceeded its
	// deadline (path blackholing, unresponsive endpoint).
	FailureClassTimeout = "timeout"
	// FailureClassTransport: a transport/protocol-specific failure
	// the other classes do not cover (SOCKS errors, core protocol
	// errors, unsupported feature) — the class that should make
	// the system prefer a DIFFERENT transport after repetition.
	FailureClassTransport = "transport"
)

// AllFailureClasses is the exhaustive failure-class domain.
var AllFailureClasses = []string{
	FailureClassDNS,
	FailureClassTCP,
	FailureClassTLS,
	FailureClassHandshake,
	FailureClassListener,
	FailureClassVerify,
	FailureClassReset,
	FailureClassTimeout,
	FailureClassTransport,
}

// ValidFailureClass reports whether v is a known failure class.
func ValidFailureClass(v string) bool {
	for _, ok := range AllFailureClasses {
		if v == ok {
			return true
		}
	}

	return false
}

// ClassifyFailure maps an observed failure description onto the
// failure-class vocabulary. The input is the same credential-free
// error text stored in LastFailureReason ("dial tcp ...: connection
// refused", "handshake timeout", "core exited during startup",
// ...). Classification is ordered: the most specific classes are
// matched first, timeout LAST — a "TLS handshake timeout" is a TLS
// failure with a deadline, but the observable failure mode (TLS
// never completing) is the actionable fact for transport selection;
// TLS likewise outranks the core-handshake class because "tls:
// handshake failure" is a TLS alert, while core readiness failures
// surface as startup/exit events. Unknown text yields ""
// (unclassified), never a wrong class.
// Unknown text yields "" (unclassified), never a wrong class.
func ClassifyFailure(reason string) string {
	text := strings.ToLower(strings.TrimSpace(reason))

	if text == "" {
		return ""
	}

	switch {
	case containsAny(text,
		"no such host", "dns", "lookup ", "name resolution",
		"servfail", "nxdomain", "resolver"):
		return FailureClassDNS

	case containsAny(text,
		"listener", "bind", "address already in use", "port in use"):
		return FailureClassListener

	// TLS is matched BEFORE the core-handshake class: a "tls: handshake
	// failure" is a TLS alert (the actionable fact for transport
	// selection is that TLS never negotiated), while core-handshake
	// failures surface as readiness/startup events, not the word
	// "handshake" alone.
	case containsAny(text,
		"tls", "certificate", "x509", "alert", "reality",
		"ech", "cipher", "ssl"):
		return FailureClassTLS

	case containsAny(text,
		"handshake timeout", "not ready", "startup timeout",
		"core exited", "process exited during startup"):
		return FailureClassHandshake

	case containsAny(text,
		"connection reset", "reset", "broken pipe", "eof",
		"connection aborted"):
		return FailureClassReset

	case containsAny(text,
		"refused", "unreachable", "no route", "network is down"):
		return FailureClassTCP

	case containsAny(text,
		"verify", "verification", "quorum", "target fetch"):
		return FailureClassVerify

	case containsAny(text,
		"socks", "proxy", "protocol", "transport", "unsupported",
		"malformed", "invalid "):
		return FailureClassTransport

	case containsAny(text,
		"timeout", "deadline", "timed out", "i/o timeout"):
		return FailureClassTimeout
	}

	return ""
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}

	return false
}

// TestHistoryLimit caps the per-config observation ring. Twelve
// entries are enough to see a trend (the ranking layer reports
// "stable over the last 12 tests") while keeping every stored record
// small: large ingests must not pay a history tax.
const TestHistoryLimit = 12

// TestObservation is one recorded test outcome.
type TestObservation struct {
	// At is the observation time (Unix milliseconds).
	At int64 `json:"at"`

	// Working reports whether the configuration served traffic.
	Working bool `json:"working"`

	// LatencyMS is the measured round-trip time (0 when failed).
	LatencyMS int64 `json:"latency_ms,omitempty"`

	// TimedOut marks failures that were timeouts — the ranking
	// layer penalises them harder than refusals.
	TimedOut bool `json:"timed_out,omitempty"`

	// Backend is the core that executed the test (informational).
	Backend string `json:"backend,omitempty"`

	// FailureClass is the v0.11.0 classification of a failed
	// observation (one of the FailureClass* constants; "" for
	// successes and unclassifiable failures). It is derived from
	// the same observed error text as LastFailureReason — never
	// invented — so per-class evidence accumulates in the bounded
	// history ring the ranking layer already scores from.
	FailureClass string `json:"failure_class,omitempty"`
}

// AppendTestObservation records one outcome in the bounded history
// ring, dropping the oldest entry beyond TestHistoryLimit. Safe on a
// nil receiver's fields; the receiver itself must be non-nil.
func (c *Config) AppendTestObservation(obs TestObservation) {
	if obs.At == 0 {
		obs.At = time.Now().UTC().UnixMilli()
	}

	c.TestHistory = append(c.TestHistory, obs)

	if excess := len(c.TestHistory) - TestHistoryLimit; excess > 0 {
		c.TestHistory = append([]TestObservation(nil),
			c.TestHistory[excess:]...)
	}
}

// FailureEvidence reports the bounded freshness/evidence tuple the
// ranking layer reasons over (v0.11.0). All values are actual
// observations: last_verified_at is the last VERIFIED-usable time
// (LastSuccessAt), last_failure_at the last failed-verification
// time, recent_failure_class the class of that failure, and
// verification_age the distance to whichever of the two is more
// recent. Zero times mean "never observed", never "unknown magic".
type FailureEvidence struct {
	LastVerifiedAt     int64
	LastFailureAt      int64
	RecentFailureClass string
	VerificationAgeMS  int64
}

// Evidence derives the bounded freshness/evidence tuple from the
// recorded observations. nowMS is the current Unix time in
// milliseconds (passed in so tests are deterministic).
func (c *Config) Evidence(nowMS int64) FailureEvidence {
	ev := FailureEvidence{
		LastVerifiedAt:     c.LastSuccessAt,
		LastFailureAt:      c.LastFailureAt,
		RecentFailureClass: c.LastFailureClass,
	}

	switch {
	case c.LastSuccessAt >= c.LastFailureAt && c.LastSuccessAt > 0:
		ev.VerificationAgeMS = nowMS - c.LastSuccessAt
	case c.LastFailureAt > 0:
		ev.VerificationAgeMS = nowMS - c.LastFailureAt
	}

	if ev.VerificationAgeMS < 0 {
		ev.VerificationAgeMS = 0
	}

	return ev
}

// ECHPEMBegin is the ONLY PEM header the pinned sing-box v1.14.0
// accepts for the ECH config list (verified empirically: "ECH
// CONFIG" and "ECHCONFIG" spellings are rejected with "invalid ECH
// configs pem").
const ECHPEMBegin = "-----BEGIN ECH CONFIGS-----"

// ECHPEMEnd closes the ECH config PEM envelope.
const ECHPEMEnd = "-----END ECH CONFIGS-----"

// normalizeECHConfig canonicalizes an ECH config value. A value that
// already carries the PEM envelope passes through (whitespace
// trimmed). A raw base64 body (the DNS HTTPS record `ech=` payload
// shape) is wrapped into the envelope. A value that is neither is
// returned unchanged and rejected later by Validate with the precise
// reason — normalization never silently mangles user input.
func normalizeECHConfig(value string) string {
	if value == "" || strings.Contains(value, ECHPEMBegin) {
		return value
	}

	body := strings.Join(strings.Fields(value), "")
	if body == "" || !isContinuedBase64(body) {
		return value
	}

	return ECHPEMBegin + "\n" + body + "\n" + ECHPEMEnd + "\n"
}

// isContinuedBase64 reports whether s is decodable standard base64
// after stripping embedded whitespace/newlines (PEM bodies are
// line-wrapped).
func isContinuedBase64(s string) bool {
	compact := strings.Join(strings.Fields(s), "")

	if len(compact) == 0 || len(compact)%4 != 0 {
		return false
	}

	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="

	for _, r := range compact {
		if !strings.ContainsRune(alphabet, r) {
			return false
		}
	}

	return true
}

// Normalize prepares a configuration for comparison and fingerprinting.
func (c *Config) Normalize() {
	c.Type = Type(strings.ToLower(strings.TrimSpace(string(c.Type))))
	c.Address = strings.ToLower(strings.TrimSpace(c.Address))
	c.Name = strings.TrimSpace(c.Name)

	c.Network = strings.ToLower(strings.TrimSpace(c.Network))
	c.Security = strings.ToLower(strings.TrimSpace(c.Security))
	c.Method = strings.ToLower(strings.TrimSpace(c.Method))

	c.ServerName = strings.TrimSpace(c.ServerName)
	c.Host = strings.TrimSpace(c.Host)
	c.Path = strings.TrimSpace(c.Path)
	c.Service = strings.TrimSpace(c.Service)

	c.FingerprintProfile = strings.ToLower(
		strings.TrimSpace(c.FingerprintProfile),
	)

	c.PublicKey = strings.TrimSpace(c.PublicKey)
	c.ShortID = strings.TrimSpace(c.ShortID)

	c.Flow = strings.ToLower(strings.TrimSpace(c.Flow))
	c.Encryption = strings.ToLower(strings.TrimSpace(c.Encryption))
	c.HeaderType = strings.ToLower(strings.TrimSpace(c.HeaderType))
	c.SpiderX = strings.TrimSpace(c.SpiderX)

	// v0.10.3 protocol-detail normalization.
	c.CongestionControl = strings.ToLower(strings.TrimSpace(c.CongestionControl))
	c.UDPRelayMode = strings.ToLower(strings.TrimSpace(c.UDPRelayMode))
	c.Obfs = strings.ToLower(strings.TrimSpace(c.Obfs))
	c.ObfsPassword = strings.TrimSpace(c.ObfsPassword)

	// v0.11.0 ECH normalization: trim, and canonicalize a raw
	// base64 ECHConfigList (the DNS HTTPS `ech=` payload shape)
	// into the PEM envelope the pinned sing-box v1.14.0 requires.
	// Already-PEM values pass through unchanged; values that are
	// neither PEM nor decodable base64 are left as-is — Validate
	// rejects them with the precise reason.
	c.ECHConfig = normalizeECHConfig(strings.TrimSpace(c.ECHConfig))
	c.ECHConfigPath = strings.TrimSpace(c.ECHConfigPath)
	c.ECHQueryServerName = strings.TrimSpace(c.ECHQueryServerName)

	c.ALPN = compactStrings(c.ALPN)

	c.PrivateKey = strings.TrimSpace(c.PrivateKey)

	// v0.11.0: a fresh failure class must stay inside the known
	// vocabulary; legacy/garbage values are dropped rather than
	// trusted (ranking reasons over classes, so an unknown class
	// would silently degrade transport preference).
	if !ValidFailureClass(c.LastFailureClass) {
		c.LastFailureClass = ""
	}

	for i := range c.AllowedIPs {
		c.AllowedIPs[i] = strings.TrimSpace(c.AllowedIPs[i])
	}

	for i := range c.DNS {
		c.DNS[i] = strings.TrimSpace(c.DNS[i])
	}

	for i := range c.InterfaceAddress {
		c.InterfaceAddress[i] = strings.TrimSpace(c.InterfaceAddress[i])
	}

	c.AllowedIPs = compactStrings(c.AllowedIPs)
	c.InterfaceAddress = compactStrings(c.InterfaceAddress)
	c.DNS = compactStrings(c.DNS)
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))

	for _, value := range values {
		value = strings.TrimSpace(value)

		if value == "" {
			continue
		}

		if _, exists := seen[value]; exists {
			continue
		}

		seen[value] = struct{}{}
		result = append(result, value)
	}

	return result
}

// Fingerprint returns a deterministic identifier for the configuration.
//
// Mutable fields such as Working, LatencyMS, TestedAt and Source are
// deliberately excluded. This allows the same configuration to be
// recognized as a duplicate even when discovered through different sources
// or tested at different times.
func (c *Config) Fingerprint() string {
	c.Normalize()

	data := strings.Join([]string{
		string(c.Type),
		c.Address,
		strconv.Itoa(c.Port),

		c.UUID,
		c.Username,
		c.Password,
		c.Method,

		c.Network,
		c.Path,
		c.Host,
		c.Service,

		c.Security,
		c.ServerName,
		c.FingerprintProfile,
		c.PublicKey,
		c.ShortID,

		strings.Join(c.AllowedIPs, ","),
		strings.Join(c.DNS, ","),
		strconv.Itoa(c.MTU),
		strconv.Itoa(c.PersistentKeepalive),
	}, "\x00")

	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// SetID calculates and stores the deterministic configuration ID.
func (c *Config) SetID() {
	c.ID = c.Fingerprint()
}
