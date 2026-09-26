package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Validate checks whether a configuration contains the minimum
// information required to be processed by a protocol engine.
//
// Validate does not perform network access and does not determine
// whether the remote server is currently working.
func (c *Config) Validate() error {
	c.Normalize()

	if c.Type == "" || c.Type == TypeUnknown {
		return fmt.Errorf("unsupported or missing protocol type")
	}

	if strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("server address is required")
	}

	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Port)
	}

	if err := validateECH(c); err != nil {
		return err
	}

	switch c.Type {
	case TypeVLESS:
		return validateVLESS(c)

	case TypeVMess:
		return validateVMess(c)

	case TypeTrojan:
		return validateTrojan(c)

	case TypeShadowsocks:
		return validateShadowsocks(c)

	case TypeHysteria, TypeHysteria2:
		return validateHysteria(c)

	case TypeTUIC:
		return validateTUIC(c)

	case TypeWireGuard:
		return validateWireGuard(c)

	case TypeSOCKS, TypeHTTP:
		return validateProxy(c)

	default:
		return fmt.Errorf("unsupported protocol: %s", c.Type)
	}
}

func validateVLESS(c *Config) error {
	if strings.TrimSpace(c.UUID) == "" {
		return fmt.Errorf("VLESS UUID is required")
	}

	return nil
}

func validateVMess(c *Config) error {
	if strings.TrimSpace(c.UUID) == "" {
		return fmt.Errorf("VMess UUID is required")
	}

	return nil
}

func validateTrojan(c *Config) error {
	if strings.TrimSpace(c.Password) == "" {
		return fmt.Errorf("Trojan password is required")
	}

	return nil
}

func validateShadowsocks(c *Config) error {
	if strings.TrimSpace(c.Method) == "" {
		return fmt.Errorf("Shadowsocks method is required")
	}

	if strings.TrimSpace(c.Password) == "" {
		return fmt.Errorf("Shadowsocks password is required")
	}

	return nil
}

func validateHysteria(c *Config) error {
	if strings.TrimSpace(c.Password) == "" &&
		strings.TrimSpace(c.UUID) == "" {
		return fmt.Errorf("Hysteria authentication is required")
	}

	// v0.10.4: the two Hysteria protocols have DIFFERENT obfs
	// semantics and must not share one validation rule.
	//
	//   - Hysteria2: the URI carries obfs=<type> + obfs-password=<pw>;
	//     the type is an ENUMERATED value ("" | salamander | gecko
	//     for sing-box v1.14). v0.10.3 only knew salamander and
	//     rejected the documented gecko.
	//   - Hysteria (v1): the format has no obfs TYPE concept — the
	//     obfuscation value IS the password string the server was
	//     configured with. Restricting it to the enum (the v0.10.3
	//     behavior) rejected every real v1 configuration whose
	//     password was not literally "salamander". Any non-empty
	//     string is accepted here and generated as the sing-box
	//     JSON string `"obfs": "..."`.
	if c.Type == TypeHysteria2 {
		if c.Obfs != "" && !ValidHysteria2Obfs(c.Obfs) {
			return fmt.Errorf("invalid Hysteria2 obfs %q (allowed: empty or %s)",
				c.Obfs, strings.Join(Hysteria2ObfsValues, ", "))
		}

		return nil
	}

	// Hysteria (v1): obfs is a free-form obfuscation password.
	// ObfsPassword is a Hysteria2 concept; nothing to validate.
	return nil
}

func validateTUIC(c *Config) error {
	if strings.TrimSpace(c.UUID) == "" {
		return fmt.Errorf("TUIC UUID is required")
	}

	if strings.TrimSpace(c.Password) == "" {
		return fmt.Errorf("TUIC password is required")
	}

	// v0.10.3: congestion control is its OWN field with its OWN value
	// domain — bbr | cubic | new_reno. It must never appear in
	// Network (the transport slot); the capability matcher would read
	// it as a transport name. An explicit value outside the domain is
	// rejected instead of silently forwarded to the core.
	if c.CongestionControl != "" && !ValidTUICCongestionControl(c.CongestionControl) {
		return fmt.Errorf("invalid TUIC congestion_control %q (allowed: %s)",
			c.CongestionControl, strings.Join(TUICCongestionControlValues, ", "))
	}

	if c.UDPRelayMode != "" && !ValidTUICUDPRelayMode(c.UDPRelayMode) {
		return fmt.Errorf("invalid TUIC udp_relay_mode %q (allowed: %s)",
			c.UDPRelayMode, strings.Join(TUICUDPRelayModeValues, ", "))
	}

	return nil
}

func validateWireGuard(c *Config) error {
	if strings.TrimSpace(c.PublicKey) == "" {
		return fmt.Errorf("WireGuard public key is required")
	}

	return nil
}

func validateProxy(c *Config) error {
	return nil
}

// validateECH enforces the v0.11.0 ECH structural rules. Every rule
// below encodes an EMPIRICALLY VERIFIED fact about the pinned
// sing-box v1.14.0 (probes re-run against the real binary; see
// docs/protocols.md for the evidence table):
//
//   - ECH rides the tls object, so it needs TLS: plain-TLS protocols
//     require security "tls"; the TLS-mandatory QUIC family
//     (hysteria/hysteria2/tuic) always qualifies; WireGuard has no
//     TLS layer at all and can never carry ECH.
//   - REALITY conflicts with ECH: the pinned binary rejects the
//     combination at startup ("Reality is conflict with ECH").
//   - ech.config must be a PEM "ECH CONFIGS" block with a base64
//     body — raw base64, hex and the "ECH CONFIG"/"ECHCONFIG"
//     header spellings are all rejected ("invalid ECH configs pem").
//     Normalize has already wrapped raw-base64 input, so a non-PEM
//     value here means the user supplied neither form.
//   - config and config_path together are ambiguous — the pinned
//     binary does not document a precedence, so FreeIran rejects the
//     combination instead of silently picking one.
//   - enabled with NEITHER config NOR config_path is accepted
//     upstream (DNS-based discovery, optionally guided by
//     query_server_name) and therefore accepted here.
func validateECH(c *Config) error {
	if !c.ECHEnabled {
		return nil
	}

	// TLS requirement.
	switch c.Type {
	case TypeHysteria, TypeHysteria2, TypeTUIC:
		// TLS-mandatory family — always eligible.
	case TypeVLESS, TypeVMess, TypeTrojan, TypeShadowsocks:
		if c.Security != string(SecurityTLS) {
			return fmt.Errorf("ECH requires TLS security (got %q; REALITY conflicts with ECH and plaintext cannot carry it)",
				c.Security)
		}
	default:
		return fmt.Errorf("ECH is not applicable to %s (no TLS layer)", c.Type)
	}

	if c.Security == string(SecurityReality) {
		return fmt.Errorf("REALITY conflicts with ECH (verified against sing-box v1.14.0: \"Reality is conflict with ECH\")")
	}

	// Config XOR config_path (both set is ambiguous).
	if c.ECHConfig != "" && c.ECHConfigPath != "" {
		return fmt.Errorf("ECH config and config_path are mutually exclusive — provide exactly one source")
	}

	if c.ECHConfig != "" && !validECHPEM(c.ECHConfig) {
		return fmt.Errorf("ECH config must be a PEM %q block with a base64 body (the pinned sing-box v1.14.0 rejects every other form)",
			ECHPEMBegin)
	}

	return nil
}

// validECHPEM reports whether value is a PEM "ECH CONFIGS" block
// whose body decodes as base64. The ECHConfigList BYTES are not
// structurally validated: the pinned sing-box v1.14.0 itself only
// PEM-parses at check time (a syntactically valid envelope with a
// garbage body passes `sing-box check`; the list is parsed at
// connection time). Claiming more validation than the core performs
// would be dishonest — the check here proves the envelope is
// exactly the form the core accepts.
func validECHPEM(value string) bool {
	value = strings.TrimSpace(value)

	if !strings.HasPrefix(value, ECHPEMBegin) || !strings.Contains(value, ECHPEMEnd) {
		return false
	}

	body := strings.TrimPrefix(value, ECHPEMBegin)
	if end := strings.Index(body, ECHPEMEnd); end >= 0 {
		body = body[:end]
	}

	body = strings.TrimSpace(body)
	if body == "" {
		return false
	}

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		decoded, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(decoded) == 0 {
			return false
		}
	}

	return true
}

// Endpoint returns the normalized host:port representation of a config.
func (c *Config) Endpoint() string {
	return net.JoinHostPort(c.Address, strconv.Itoa(c.Port))
}
