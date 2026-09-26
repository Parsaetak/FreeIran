package core

import (
	"sort"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// Capabilities is the declarative feature model of one backend. Each
// backend declares the protocols, transports, security mechanisms and
// platform notes it supports; selection and validation consult this
// model instead of scattering protocol knowledge through the app.
//
// The declarations are verified against real binaries (see
// engine/core/*/config_test.go and docs/development.md for the pinned
// versions) — they are facts, not aspirations.
type Capabilities struct {
	// Protocols lists supported outbound protocols.
	Protocols []config.Type

	// Transports lists supported transports (network layer values).
	Transports []string

	// Securities lists supported security layers.
	Securities []string

	// Flows lists supported VLESS flow values ("" = no flow support).
	Flows []string

	// TLSMandatory lists protocols that only operate over TLS on
	// this backend (e.g. trojan on sing-box).
	TLSMandatory []config.Type

	// ECH (v0.11.0) declares whether the backend can execute a
	// configuration carrying Encrypted Client Hello fields. A
	// backend that does not declare ECH silently IGNORES the ECH
	// layer if handed such a config (verified: V2Ray 5.53.0
	// ignores an echConfigList field entirely; Xray 26.3.27 accepts
	// the field in schema but does not validate its content at
	// config-check time) — so the capability matcher routes
	// ECH-enabled configurations ONLY to backends that both
	// declare ECH and were schema-verified against the pinned
	// real binary. Today that is sing-box 1.14.0 only. This is a
	// declared FACT, not an aspiration; see docs/protocols.md
	// for the per-core evidence table.
	ECH bool

	// Notes are human-readable capability notes surfaced in
	// diagnostics (e.g. "REALITY: yes (xray verified v26)").
	Notes []string
}

// Matches reports whether a normalized configuration's protocol,
// transport, security and flow are all within the declared
// capabilities. It is the capability resolver used by backend
// Supports implementations.
func (c Capabilities) Matches(cfg config.Config) bool {
	return c.SupportsProtocol(cfg.Type) &&
		c.SupportsTransport(cfg.Network) &&
		c.SupportsSecurity(cfg.Security) &&
		c.SupportsFlow(cfg.Flow) &&
		c.SupportsECH(cfg)
}

// SupportsECH reports whether the backend may execute a
// configuration carrying ECH fields. An ECH-disabled configuration
// needs no ECH support; an ECH-enabled one is routed only to
// ECH-declared backends — anything else would silently connect
// WITHOUT the encrypted client hello the user configured.
func (c Capabilities) SupportsECH(cfg config.Config) bool {
	return !cfg.ECHEnabled || c.ECH
}

// SupportsProtocol reports whether the backend declares a protocol.
func (c Capabilities) SupportsProtocol(t config.Type) bool {
	for _, p := range c.Protocols {
		if p == t {
			return true
		}
	}

	return false
}

// SupportsTransport reports whether the backend declares a
// transport ("" and "tcp" are equivalent: plain TCP is the default
// transport).
func (c Capabilities) SupportsTransport(network string) bool {
	if network == "" || network == string(config.NetworkTCP) {
		network = string(config.NetworkTCP)
	}

	for _, t := range c.Transports {
		if t == network {
			return true
		}
	}

	return false
}

// SupportsSecurity reports whether the backend declares a security
// layer ("" and "none" are equivalent).
func (c Capabilities) SupportsSecurity(security string) bool {
	if security == "" {
		security = string(config.SecurityNone)
	}

	for _, s := range c.Securities {
		if s == security {
			return true
		}
	}

	return false
}

// SupportsFlow reports whether the backend declares a VLESS flow
// value. An empty cfg.Flow needs no flow support.
func (c Capabilities) SupportsFlow(flow string) bool {
	if flow == "" {
		return true
	}

	for _, f := range c.Flows {
		if f == flow {
			return true
		}
	}

	return false
}

// RequiresTLS reports whether a protocol is declared TLS-mandatory.
func (c Capabilities) RequiresTLS(t config.Type) bool {
	for _, p := range c.TLSMandatory {
		if p == t {
			return true
		}
	}

	return false
}

// Summary renders a compact credential-free capability description
// for diagnostics and the backend cards in the UI.
func (c Capabilities) Summary() string {
	parts := make([]string, 0, 3)

	if len(c.Protocols) > 0 {
		protocols := make([]string, 0, len(c.Protocols))

		for _, p := range c.Protocols {
			protocols = append(protocols, string(p))
		}

		parts = append(parts, strings.Join(protocols, "/"))
	}

	if len(c.Transports) > 0 {
		parts = append(parts, strings.Join(c.Transports, "/"))
	}

	if len(c.Securities) > 1 ||
		(len(c.Securities) == 1 && c.Securities[0] != string(config.SecurityNone)) {
		parts = append(parts, strings.Join(c.Securities, "/"))
	}

	return strings.Join(parts, " ")
}

// sortedStrings returns a deterministic copy for display.
func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)

	return out
}
