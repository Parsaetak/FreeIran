package parser

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// Parser converts externally published configuration data into the
// FreeIran universal configuration model.
type Parser struct{}

// New creates a parser.
func New() *Parser {
	return &Parser{}
}

// Parse accepts raw configuration data and returns normalized,
// structurally valid configurations.
//
// The parser currently supports:
//   - VLESS URLs
//   - VMess URLs
//   - Trojan URLs
//   - Shadowsocks URLs
//
// Subscription decoding and additional protocol formats will be added
// without changing this public entry point.
func (p *Parser) Parse(data []byte) ([]config.Config, error) {
	text := strings.TrimSpace(string(data))

	if text == "" {
		return nil, fmt.Errorf("configuration input is empty")
	}

	// First try to interpret the input as a JSON configuration.
	if configs, ok, err := parseJSON(text); ok {
		if err != nil {
			return nil, err
		}

		return normalizeAndValidate(configs)
	}

	// Otherwise treat the input as a list of configuration URLs.
	return p.parseURLs(text)
}

// parseURLs extracts supported configuration URLs from newline-separated
// or whitespace-separated input.
func (p *Parser) parseURLs(text string) ([]config.Config, error) {
	lines := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r'
	})

	var configs []config.Config
	var parseErrors []string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		// Some subscription sources return base64-encoded content.
		if decoded, ok := decodeBase64(line); ok {
			decodedLines := strings.FieldsFunc(decoded, func(r rune) bool {
				return r == '\n' || r == '\r'
			})

			for _, decodedLine := range decodedLines {
				decodedLine = strings.TrimSpace(decodedLine)

				if decodedLine == "" {
					continue
				}

				cfg, err := parseURL(decodedLine)
				if err != nil {
					parseErrors = append(parseErrors, err.Error())
					continue
				}

				configs = append(configs, cfg)
			}

			continue
		}

		cfg, err := parseURL(line)
		if err != nil {
			parseErrors = append(parseErrors, err.Error())
			continue
		}

		configs = append(configs, cfg)
	}

	configs, validationErrors := normalizeAndValidate(configs)

	for _, err := range validationErrors {
		parseErrors = append(parseErrors, err.Error())
	}

	if len(configs) == 0 {
		if len(parseErrors) > 0 {
			return nil, fmt.Errorf(
				"no valid configurations found: %s",
				strings.Join(parseErrors, "; "),
			)
		}

		return nil, fmt.Errorf("no configurations found")
	}

	return configs, nil
}

// parseURL dispatches a configuration URL to the appropriate protocol parser.
func parseURL(raw string) (config.Config, error) {
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return config.Config{}, fmt.Errorf("empty configuration URL")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return config.Config{}, fmt.Errorf("invalid configuration URL: %w", err)
	}

	switch strings.ToLower(u.Scheme) {
	case "vless":
		return parseVLESS(u)

	case "vmess":
		return parseVMess(u)

	case "trojan":
		return parseTrojan(u)

	case "ss":
		return parseShadowsocks(u)

	default:
		return config.Config{}, fmt.Errorf(
			"unsupported configuration scheme: %q",
			u.Scheme,
		)
	}
}

// parseVLESS parses a VLESS URI.
func parseVLESS(u *url.URL) (config.Config, error) {
	if u.Host == "" {
		return config.Config{}, fmt.Errorf("VLESS URL has no server address")
	}

	port := u.Port()

	if port == "" {
		return config.Config{}, fmt.Errorf("VLESS URL has no port")
	}

	portNumber := parsePort(port)
	if portNumber == 0 {
		return config.Config{}, fmt.Errorf("invalid VLESS port: %q", port)
	}

	cfg := config.Config{
		Type:       config.TypeVLESS,
		Address:    u.Hostname(),
		Port:       portNumber,
		UUID:       u.User.Username(),
		Network:    u.Query().Get("type"),
		Security:   u.Query().Get("security"),
		ServerName: u.Query().Get("sni"),
		Host:       u.Query().Get("host"),
		Path:       u.Query().Get("path"),
		Service:    u.Query().Get("serviceName"),
		PublicKey:  u.Query().Get("pbk"),
		ShortID:    u.Query().Get("sid"),
		FingerprintProfile: u.Query().Get("fp"),
		Name:       u.Fragment,
	}

	return cfg, nil
}

// parseVMess parses a VMess URI.
//
// VMess links commonly contain base64-encoded JSON.
func parseVMess(u *url.URL) (config.Config, error) {
	payload := strings.TrimPrefix(u.String(), "vmess://")

	decoded, err := decodeBase64Strict(payload)
	if err != nil {
		return config.Config{}, fmt.Errorf("invalid VMess payload: %w", err)
	}

	var data struct {
		PS   string `json:"ps"`
		Add  string `json:"add"`
		Port any    `json:"port"`
		ID   string `json:"id"`
		AID  any    `json:"aid"`
		Net  string `json:"net"`
		Type string `json:"type"`
		Host string `json:"host"`
		Path string `json:"path"`
		TLS  string `json:"tls"`
		SNI  string `json:"sni"`
	}

	if err := json.Unmarshal([]byte(decoded), &data); err != nil {
		return config.Config{}, fmt.Errorf("invalid VMess JSON: %w", err)
	}

	port := normalizePort(data.Port)

	if data.Add == "" {
		return config.Config{}, fmt.Errorf("VMess configuration has no server address")
	}

	if port == 0 {
		return config.Config{}, fmt.Errorf("VMess configuration has no valid port")
	}

	return config.Config{
		Type:       config.TypeVMess,
		Address:    data.Add,
		Port:       port,
		UUID:       data.ID,
		Network:    data.Net,
		Host:       data.Host,
		Path:       data.Path,
		Security:   data.TLS,
		ServerName: data.SNI,
		Name:       data.PS,
	}, nil
}

// parseTrojan parses a Trojan URI.
func parseTrojan(u *url.URL) (config.Config, error) {
	if u.Host == "" {
		return config.Config{}, fmt.Errorf("Trojan URL has no server address")
	}

	port := parsePort(u.Port())

	if port == 0 {
		return config.Config{}, fmt.Errorf("invalid Trojan port")
	}

	password := ""

	if u.User != nil {
		password, _ = u.User.Password()
	}

	return config.Config{
		Type:       config.TypeTrojan,
		Address:    u.Hostname(),
		Port:       port,
		Password:   password,
		Network:    u.Query().Get("type"),
		Security:   u.Query().Get("security"),
		ServerName: u.Query().Get("sni"),
		Host:       u.Query().Get("host"),
		Path:       u.Query().Get("path"),
		Service:    u.Query().Get("serviceName"),
		Name:       u.Fragment,
	}, nil
}

// parseShadowsocks parses an SS URI.
//
// Both modern SIP002-style URLs and common base64 forms are supported
// incrementally. Full method-specific handling belongs in the SS adapter.
func parseShadowsocks(u *url.URL) (config.Config, error) {
	if u.Host == "" {
		return config.Config{}, fmt.Errorf(
			"Shadowsocks URL has no server address",
		)
	}

	port := parsePort(u.Port())

	if port == 0 {
		return config.Config{}, fmt.Errorf(
			"invalid Shadowsocks port",
		)
	}

	username := ""

	if u.User != nil {
		username = u.User.Username()
	}

	return config.Config{
		Type:     config.TypeShadowsocks,
		Address:  u.Hostname(),
		Port:     port,
		Method:   username,
		Password: mustPassword(u),
		Name:     u.Fragment,
	}, nil
}

// normalizeAndValidate normalizes configurations, assigns IDs and removes
// invalid entries.
func normalizeAndValidate(
	configs []config.Config,
) ([]config.Config, []error) {
	valid := make([]config.Config, 0, len(configs))
	var errors []error

	seen := make(map[string]struct{}, len(configs))

	for i := range configs {
		cfg := configs[i]

		cfg.Normalize()

		if err := cfg.Validate(); err != nil {
			errors = append(
				errors,
				fmt.Errorf("configuration %d: %w", i, err),
			)
			continue
		}

		cfg.SetID()

		if _, exists := seen[cfg.ID]; exists {
			continue
		}

		seen[cfg.ID] = struct{}{}
		valid = append(valid, cfg)
	}

	return valid, errors
}

func parsePort(value string) int {
	if value == "" {
		return 0
	}

	var port int

	for _, r := range value {
		if r < '0' || r > '9' {
			return 0
		}

		port = port*10 + int(r-'0')

		if port > 65535 {
			return 0
		}
	}

	return port
}

func normalizePort(value any) int {
	switch v := value.(type) {
	case float64:
		port := int(v)

		if port >= 1 && port <= 65535 {
			return port
		}

	case string:
		return parsePort(v)
	}

	return 0
}

func decodeBase64(value string) (string, bool) {
	value = strings.TrimSpace(value)

	if value == "" {
		return "", false
	}

	decoded, err := decodeBase64Strict(value)

	if err != nil {
		return "", false
	}

	// Avoid treating ordinary configuration URLs as base64 accidentally.
	if !strings.Contains(decoded, "://") &&
		!strings.Contains(decoded, "\n") {
		return "", false
	}

	return decoded, true
}

func decodeBase64Strict(value string) (string, error) {
	value = strings.TrimSpace(value)

	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}

	var lastErr error

	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(value)

		if err == nil {
			return string(decoded), nil
		}

		lastErr = err
	}

	return "", lastErr
}

func mustPassword(u *url.URL) string {
	if u.User == nil {
		return ""
	}

	password, _ := u.User.Password()
	return password
}
