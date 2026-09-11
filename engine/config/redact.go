package config

import (
	"net"
	"strconv"
	"strings"
)

// SecretPlaceholder replaces redacted credential material in logs and
// diagnostics output.
const SecretPlaceholder = "***"

// SecretFields lists the normalized field values that carry credentials
// for a configuration. Backends, the connection manager and diagnostics
// pass these values through redaction before any output.
func (c Config) SecretFields() []string {
	secrets := make([]string, 0, 4)

	for _, value := range []string{
		c.UUID,
		c.Password,
		c.PublicKey,
		c.PrivateKey,
	} {
		if strings.TrimSpace(value) != "" {
			secrets = append(secrets, value)
		}
	}

	return secrets
}

// Redacted returns a copy of the configuration with credential
// material replaced by SecretPlaceholder. Runtime fields (Working,
// LatencyMS, TestedAt) and identity fields (Type, Address, Port,
// transport, security) survive so the copy remains useful for
// diagnostics and the configuration-details view.
func (c Config) Redacted() Config {
	redacted := c

	if redacted.UUID != "" {
		redacted.UUID = SecretPlaceholder
	}

	if redacted.Password != "" {
		redacted.Password = SecretPlaceholder
	}

	if redacted.PublicKey != "" {
		redacted.PublicKey = SecretPlaceholder
	}

	if redacted.PrivateKey != "" {
		redacted.PrivateKey = SecretPlaceholder
	}

	return redacted
}

// DisplayURL renders a redacted share-style URL for logs and status
// output: the credential is replaced but the endpoint and transport
// remain visible for operators, e.g.
//
//	vless://***@example.com:443 (tls,ws)
func (c Config) DisplayURL() string {
	scheme := string(c.Type)

	if scheme == "" || scheme == string(TypeUnknown) {
		scheme = "config"
	}

	host := c.Address
	if host == "" {
		host = "unknown"
	}

	endpoint := net.JoinHostPort(host, strconv.Itoa(c.Port))

	parts := make([]string, 0, 2)

	if c.Security != "" && c.Security != string(SecurityNone) {
		parts = append(parts, c.Security)
	}

	if c.Network != "" && c.Network != string(NetworkTCP) {
		parts = append(parts, c.Network)
	}

	if len(parts) > 0 {
		return scheme + "://" + SecretPlaceholder + "@" + endpoint +
			" (" + strings.Join(parts, ",") + ")"
	}

	return scheme + "://" + SecretPlaceholder + "@" + endpoint
}

// DisplayString renders a one-line redacted description of the
// configuration suitable for diagnostics, selection reasons and state
// snapshots. It never includes credentials.
func (c Config) DisplayString() string {
	return c.DisplayURL()
}
