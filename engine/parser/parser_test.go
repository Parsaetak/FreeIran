package parser

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
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
// structurally valid, deduplicated configurations.
func (p *Parser) Parse(data []byte) ([]config.Config, error) {
	text := strings.TrimSpace(string(data))

	if text == "" {
		return nil, fmt.Errorf("configuration input is empty")
	}

	// JSON is checked first because some sources publish structured
	// configuration objects rather than URI subscriptions.
	if configs, recognized, err := parseJSON(text); recognized {
		if err != nil {
			return nil, err
		}

		return p.finalize(configs)
	}

	return p.parseURLs(text)
}

// parseJSON attempts to interpret input as a JSON configuration source.
//
// The function returns:
//   - configs
//   - recognized: whether the input looked like JSON
//   - error: decoding error, if any
func parseJSON(text string) ([]config.Config, bool, error) {
	trimmed := strings.TrimSpace(text)

	if !strings.HasPrefix(trimmed, "{") &&
		!strings.HasPrefix(trimmed, "[") {
		return nil, false, nil
	}

	var raw any

	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, true, fmt.Errorf("invalid JSON configuration: %w", err)
	}

	var configs []config.Config

	switch value := raw.(type) {
	case map[string]any:
		cfg, ok := configFromJSON(value)

		if !ok {
			return nil, true, fmt.Errorf(
				"JSON does not contain a supported configuration object",
			)
		}

		configs = append(configs, cfg)

	case []any:
		for _, item := range value {
			object, ok := item.(map[string]any)

			if !ok {
				continue
			}

			if cfg, ok := configFromJSON(object); ok {
				configs = append(configs, cfg)
			}
		}

	default:
		return nil, true, fmt.Errorf(
			"unsupported JSON configuration structure",
		)
	}

	return configs, true, nil
}

// configFromJSON converts a simple normalized configuration object.
//
// This deliberately supports the universal Config representation rather
// than attempting to interpret every possible Xray/sing-box JSON schema.
// Dedicated source adapters will handle those richer schemas later.
func configFromJSON(value map[string]any) (config.Config, bool) {
	getString := func(key string) string {
		value, ok := value[key]
		if !ok {
			return ""
		}

		result, ok := value.(string)
		if !ok {
			return ""
		}

		return result
	}

	getInt := func(key string) int {
		value, ok := value[key]
		if !ok {
			return 0
		}

		switch v := value.(type) {
		case float64:
			return int(v)

		case string:
			return parsePort(v)

		default:
			return 0
		}
	}

	protocol := strings.ToLower(
		strings.TrimSpace(getString("type")),
	)

	if protocol == "" {
		return config.Config{}, false
	}

	cfg := config.Config{
		Type:               config.Type(protocol),
		ID:                 getString("id"),
		Name:               getString("name"),
		Address:            getString("address"),
		Port:               getInt("port"),
		UUID:               getString("uuid"),
		Username:           getString("username"),
		Password:           getString("password"),
		Method:             getString("method"),
		Network:            getString("network"),
		Path:               getString("path"),
		Host:               getString("host"),
		Service:            getString("service"),
		Security:           getString("security"),
		ServerName:         getString("server_name"),
		FingerprintProfile: getString("fingerprint"),
		PublicKey:          getString("public_key"),
		ShortID:            getString("short_id"),
		Source:             getString("source"),
	}

	return cfg, true
}

// parseURLs extracts supported configuration URLs from newline-separated
// input. Invalid entries are ignored when at least one valid configuration
// remains.
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

		// A complete subscription may itself be base64 encoded.
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

	finalized, validationErrors := p.finalize(configs)

	for _, err := range validationErrors {
		parseErrors = append(parseErrors, err.Error())
	}

	if len(finalized) == 0 {
		if len(parseErrors) > 0 {
			return nil, fmt.Errorf(
				"no valid configurations found: %s",
				strings.Join(parseErrors, "; "),
			)
		}

		return nil, fmt.Errorf("no configurations found")
	}

	return finalized, nil
}

// finalize normalizes, validates, assigns IDs and deduplicates configurations.
func (p *Parser) finalize(
	configs []config.Config,
) ([]config.Config, []error) {
	valid := make([]config.Config, 0, len(configs))
	errors := make([]error, 0)

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

// parseURL dispatches a URI to its protocol parser.
func parseURL(raw string) (config.Config, error) {
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return config.Config{}, fmt.Errorf("empty configuration URL")
	}

	u, err := url.Parse(raw)

	if err != nil {
		return config.Config{}, fmt.Errorf(
			"invalid configuration URL: %w",
			err,
		)
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
		return config.Config{}, fmt.Errorf(
			"VLESS URL has no server address",
		)
	}

	port := parsePort(u.Port())

	if port == 0 {
		return config.Config{}, fmt.Errorf(
			"invalid VLESS port",
		)
	}

	if u.User == nil || strings.TrimSpace(u.User.Username()) == "" {
		return config.Config{}, fmt.Errorf(
			"VLESS URL has no UUID",
		)
	}

	query := u.Query()

	return config.Config{
		Type:               config.TypeVLESS,
		Address:            u.Hostname(),
		Port:               port,
		UUID:               u.User.Username(),
		Network:            query.Get("type"),
		Security:           query.Get("security"),
		ServerName:         query.Get("sni"),
		Host:               query.Get("host"),
		Path:               query.Get("path"),
		Service:            query.Get("serviceName"),
		PublicKey:          query.Get("pbk"),
		ShortID:            query.Get("sid"),
		FingerprintProfile: query.Get("fp"),
		Name:               u.Fragment,
	}, nil
}

// parseVMess parses the common base64-encoded VMess JSON format.
func parseVMess(u *url.URL) (config.Config, error) {
	payload := strings.TrimPrefix(u.String(), "vmess://")

	decoded, err := decodeBase64Strict(payload)

	if err != nil {
		return config.Config{}, fmt.Errorf(
			"invalid VMess payload: %w",
			err,
		)
	}

	var data struct {
		PS   string `json:"ps"`
		Add  string `json:"add"`
		Port any    `json:"port"`
		ID   string `json:"id"`
		Net  string `json:"net"`
		Host string `json:"host"`
		Path string `json:"path"`
		TLS  string `json:"tls"`
		SNI  string `json:"sni"`
		Type string `json:"type"`
	}

	if err := json.Unmarshal([]byte(decoded), &data); err != nil {
		return config.Config{}, fmt.Errorf(
			"invalid VMess JSON: %w",
			err,
		)
	}

	port := normalizePort(data.Port)

	if strings.TrimSpace(data.Add) == "" {
		return config.Config{}, fmt.Errorf(
			"VMess configuration has no server address",
		)
	}

	if port == 0 {
		return config.Config{}, fmt.Errorf(
			"VMess configuration has no valid port",
		)
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
		return config.Config{}, fmt.Errorf(
			"Trojan URL has no server address",
		)
	}

	port := parsePort(u.Port())

	if port == 0 {
		return config.Config{}, fmt.Errorf(
			"invalid Trojan port",
		)
	}

	if u.User == nil {
		return config.Config{}, fmt.Errorf(
			"Trojan URL has no password",
		)
	}

	password, _ := u.User.Password()

	query := u.Query()

	return config.Config{
		Type:       config.TypeTrojan,
		Address:    u.Hostname(),
		Port:       port,
		Password:   password,
		Network:    query.Get("type"),
		Security:   query.Get("security"),
		ServerName: query.Get("sni"),
		Host:       query.Get("host"),
		Path:       query.Get("path"),
		Service:    query.Get("serviceName"),
		Name:       u.Fragment,
	}, nil
}

// parseShadowsocks parses a standard SIP002-style SS URI.
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

	if u.User == nil {
		return config.Config{}, fmt.Errorf(
			"Shadowsocks URL has no credentials",
		)
	}

	method := u.User.Username()
	password, _ := u.User.Password()

	return config.Config{
		Type:     config.TypeShadowsocks,
		Address:  u.Hostname(),
		Port:     port,
		Method:   method,
		Password: password,
		Name:     u.Fragment,
	}, nil
}

func parsePort(value string) int {
	value = strings.TrimSpace(value)

	if value == "" {
		return 0
	}

	port, err := strconv.Atoi(value)

	if err != nil || port < 1 || port > 65535 {
		return 0
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

	case json.Number:
		return parsePort(string(v))
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

	// Prevent ordinary URLs or arbitrary strings from being treated as
	// subscriptions.
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
