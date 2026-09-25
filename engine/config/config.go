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

	c.ALPN = compactStrings(c.ALPN)

	c.PrivateKey = strings.TrimSpace(c.PrivateKey)

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
