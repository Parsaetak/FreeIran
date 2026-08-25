package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
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

// Config is the normalized representation of a VPN/proxy configuration.
//
// Parsers convert external formats into Config objects.
// Protocol engines later convert Config objects into core-specific
// configurations.
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

	// Source information.
	Source string `json:"source,omitempty"`

	// Runtime information.
	Working   bool  `json:"working"`
	LatencyMS int64 `json:"latency_ms,omitempty"`
	TestedAt  int64 `json:"tested_at,omitempty"`
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
	}, "\x00")

	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// SetID calculates and stores the deterministic configuration ID.
func (c *Config) SetID() {
	c.ID = c.Fingerprint()
}
