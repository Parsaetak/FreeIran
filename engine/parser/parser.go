package parser

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
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
// structurally valid and deduplicated configurations.
func (p *Parser) Parse(data []byte) ([]config.Config, error) {
	text := strings.TrimSpace(string(data))

	if text == "" {
		return nil, fmt.Errorf("configuration input is empty")
	}

	// Native WireGuard configurations use INI-style sections such as
	// [Interface] and [Peer]. They must be detected before JSON because
	// a WireGuard configuration also begins with '['.
	if looksLikeWireGuardConfig(text) {
		return p.parseURLs(text)
	}

	// JSON is checked before URI subscriptions because some sources
	// publish structured configuration objects.
	if configs, recognized, err := parseJSON(text); recognized {
		if err != nil {
			return nil, err
		}

		finalized, validationErrors := p.finalize(configs)

		if len(finalized) == 0 {
			if len(validationErrors) > 0 {
				return nil, fmt.Errorf(
					"no valid configurations found: %s",
					formatErrors(validationErrors),
				)
			}

			return nil, fmt.Errorf("no configurations found")
		}

		return finalized, nil
	}

	return p.parseURLs(text)
}

// parseJSON attempts to interpret input as a JSON configuration source.
//
// The function returns:
//   - configurations
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
		return nil, true, fmt.Errorf(
			"invalid JSON configuration: %w",
			err,
		)
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

// configFromJSON converts the universal FreeIran configuration
// representation from a JSON object.
//
// Rich Xray/sing-box JSON schemas will be handled by dedicated adapters
// rather than being forced into this generic representation.
func configFromJSON(value map[string]any) (config.Config, bool) {
	getString := func(key string) string {
		raw, ok := value[key]
		if !ok {
			return ""
		}

		result, ok := raw.(string)
		if !ok {
			return ""
		}

		return result
	}

	getInt := func(key string) int {
		raw, ok := value[key]
		if !ok {
			return 0
		}

		switch v := raw.(type) {
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
// input. Invalid entries are retained as errors internally but do not
// prevent valid configurations from being returned.
func (p *Parser) parseURLs(text string) ([]config.Config, error) {
	lines := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r'
	})

	var configs []config.Config
	var parseErrors []error

	if looksLikeWireGuardConfig(text) {
	cfg, err := parseWireGuardConfig(text)

	if err != nil {
		return nil, fmt.Errorf(
			"invalid WireGuard configuration: %w",
			err,
		)
	}

	finalized, validationErrors := p.finalize(
		[]config.Config{cfg},
	)

	if len(finalized) == 0 {
		return nil, fmt.Errorf(
			"no valid WireGuard configurations found: %s",
			formatErrors(validationErrors),
		)
	}

	return finalized, nil
}

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		// First attempt to interpret the complete line as a protocol URI.
		// This is important for vmess:// because its payload itself is
		// base64 encoded.
		if isSupportedScheme(line) {
			cfg, err := parseURL(line)

			if err != nil {
				parseErrors = append(parseErrors, err)
				continue
			}

			configs = append(configs, cfg)
			continue
		}

		// If the line is not a recognized URI, it may be a complete
		// base64-encoded subscription.
		if decoded, ok := decodeBase64Subscription(line); ok {
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
					parseErrors = append(parseErrors, err)
					continue
				}

				configs = append(configs, cfg)
			}

			continue
		}

		if strings.Contains(line, "://") {
	u, err := url.Parse(line)

	if err == nil && u.Scheme != "" {
		parseErrors = append(
			parseErrors,
			fmt.Errorf(
				"unsupported configuration scheme: %q",
				u.Scheme,
			),
		)
		continue
	}
}

parseErrors = append(
	parseErrors,
	fmt.Errorf("unsupported configuration input: %q", line),
)
	}

	finalized, validationErrors := p.finalize(configs)

	parseErrors = append(parseErrors, validationErrors...)

	if len(finalized) == 0 {
		if len(parseErrors) > 0 {
			return nil, fmt.Errorf(
				"no valid configurations found: %s",
				formatErrors(parseErrors),
			)
		}

		return nil, fmt.Errorf("no configurations found")
	}

	return finalized, nil
}

// finalize normalizes, validates, assigns IDs and deduplicates
// configurations.
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

// parseURL dispatches a URI to the appropriate protocol parser.
func parseURL(raw string) (config.Config, error) {
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return config.Config{}, fmt.Errorf(
			"empty configuration URL",
		)
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

case "hysteria":
	return parseHysteria(u)

case "hysteria2", "hy2":
	return parseHysteria2(u)

case "tuic":
	return parseTUIC(u)

case "wireguard", "wg":
	return parseWireGuardURL(u)

case "socks", "socks4", "socks4a", "socks5":
	return parseSOCKS(u)

case "http", "https":
	return parseHTTP(u)

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

	if u.User == nil ||
		strings.TrimSpace(u.User.Username()) == "" {
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
//
// Trojan commonly uses:
//
//	trojan://password@host:port
//
// Some producers may use:
//
//	trojan://username:password@host:port
//
// For the standard form, the URL username is the Trojan password.
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

	password := u.User.Username()

	// Standard Trojan URI:
	//
	// trojan://password@host:port
	//
	// In this form Username() contains the password.
	if suppliedPassword, hasPassword := u.User.Password(); hasPassword {
		// Also accept username:password@host for compatibility with
		// non-standard producers. The password component takes
		// precedence when explicitly supplied.
		password = suppliedPassword
	}

	password = strings.TrimSpace(password)

	if password == "" {
		return config.Config{}, fmt.Errorf(
			"Trojan URL has no password",
		)
	}

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

	if strings.TrimSpace(method) == "" {
		return config.Config{}, fmt.Errorf(
			"Shadowsocks method is empty",
		)
	}

	if strings.TrimSpace(password) == "" {
		return config.Config{}, fmt.Errorf(
			"Shadowsocks password is empty",
		)
	}

	return config.Config{
		Type:     config.TypeShadowsocks,
		Address:  u.Hostname(),
		Port:     port,
		Method:   method,
		Password: password,
		Name:     u.Fragment,
	}, nil
}

// parseHysteria parses a Hysteria URI.
func parseHysteria(u *url.URL) (config.Config, error) {
	if u.Host == "" {
		return config.Config{}, fmt.Errorf(
			"Hysteria URL has no server address",
		)
	}

	port := parsePort(u.Port())

	if port == 0 {
		return config.Config{}, fmt.Errorf(
			"invalid Hysteria port",
		)
	}

	if u.User == nil {
		return config.Config{}, fmt.Errorf(
			"Hysteria URL has no authentication",
		)
	}

	password := u.User.Username()

	if suppliedPassword, hasPassword := u.User.Password(); hasPassword {
		password = suppliedPassword
	}

	password = strings.TrimSpace(password)

	if password == "" {
		return config.Config{}, fmt.Errorf(
			"Hysteria authentication is empty",
		)
	}

	query := u.Query()

	return config.Config{
		Type:       config.TypeHysteria,
		Address:    u.Hostname(),
		Port:       port,
		Password:   password,
		ServerName: query.Get("sni"),
		Security:   query.Get("obfs"),
		Host:       query.Get("obfs-password"),
		Name:       u.Fragment,
	}, nil
}

// parseHysteria2 parses a Hysteria 2 URI.
//
// Both hysteria2:// and hy2:// are accepted by parseURL().
func parseHysteria2(u *url.URL) (config.Config, error) {
	if u.Host == "" {
		return config.Config{}, fmt.Errorf(
			"Hysteria2 URL has no server address",
		)
	}

	port := parsePort(u.Port())

	if port == 0 {
		return config.Config{}, fmt.Errorf(
			"invalid Hysteria2 port",
		)
	}

	if u.User == nil {
		return config.Config{}, fmt.Errorf(
			"Hysteria2 URL has no authentication",
		)
	}

	password := u.User.Username()

	if suppliedPassword, hasPassword := u.User.Password(); hasPassword {
		password = suppliedPassword
	}

	password = strings.TrimSpace(password)

	if password == "" {
		return config.Config{}, fmt.Errorf(
			"Hysteria2 authentication is empty",
		)
	}

	query := u.Query()

	return config.Config{
		Type:       config.TypeHysteria2,
		Address:    u.Hostname(),
		Port:       port,
		Password:   password,
		ServerName: query.Get("sni"),
		Security:   query.Get("obfs"),
		Host:       query.Get("obfs-password"),
		Name:       u.Fragment,
	}, nil
}

// parseTUIC parses a TUIC URI.
func parseTUIC(u *url.URL) (config.Config, error) {
	if u.Host == "" {
		return config.Config{}, fmt.Errorf(
			"TUIC URL has no server address",
		)
	}

	port := parsePort(u.Port())

	if port == 0 {
		return config.Config{}, fmt.Errorf(
			"invalid TUIC port",
		)
	}

	if u.User == nil {
		return config.Config{}, fmt.Errorf(
			"TUIC URL has no credentials",
		)
	}

	uuid := strings.TrimSpace(u.User.Username())

	if uuid == "" {
		return config.Config{}, fmt.Errorf(
			"TUIC UUID is empty",
		)
	}

	password, hasPassword := u.User.Password()

	if !hasPassword || strings.TrimSpace(password) == "" {
		return config.Config{}, fmt.Errorf(
			"TUIC password is empty",
		)
	}

	query := u.Query()

	return config.Config{
		Type:       config.TypeTUIC,
		Address:    u.Hostname(),
		Port:       port,
		UUID:       uuid,
		Password:   password,
		ServerName: query.Get("sni"),
		Network:    query.Get("congestion_control"),
		Name:       u.Fragment,
	}, nil
}

// parseSOCKS parses SOCKS4, SOCKS4a and SOCKS5 URIs.
func parseSOCKS(u *url.URL) (config.Config, error) {
	if u.Host == "" {
		return config.Config{}, fmt.Errorf(
			"SOCKS URL has no server address",
		)
	}

	port := parsePort(u.Port())

	if port == 0 {
		return config.Config{}, fmt.Errorf(
			"invalid SOCKS port",
		)
	}

	cfg := config.Config{
		Type:    config.TypeSOCKS,
		Address: u.Hostname(),
		Port:    port,
		Name:    u.Fragment,
	}

	if u.User != nil {
		cfg.Username = u.User.Username()

		if password, ok := u.User.Password(); ok {
			cfg.Password = password
		}
	}

	return cfg, nil
}

// parseHTTP parses HTTP and HTTPS proxy URIs.
func parseHTTP(u *url.URL) (config.Config, error) {
	if u.Host == "" {
		return config.Config{}, fmt.Errorf(
			"HTTP URL has no server address",
		)
	}

	port := parsePort(u.Port())

	if port == 0 {
		if strings.EqualFold(u.Scheme, "https") {
			port = 443
		} else {
			port = 80
		}
	}

	cfg := config.Config{
		Type:    config.TypeHTTP,
		Address: u.Hostname(),
		Port:    port,
		Name:    u.Fragment,
	}

	if strings.EqualFold(u.Scheme, "https") {
		cfg.Security = "tls"
	}

	if u.User != nil {
		cfg.Username = u.User.Username()

		if password, ok := u.User.Password(); ok {
			cfg.Password = password
		}
	}

	return cfg, nil
}

// isSupportedScheme determines whether a line is directly parseable as
// a supported protocol URI.
func isSupportedScheme(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))

	return strings.HasPrefix(lower, "vless://") ||
		strings.HasPrefix(lower, "vmess://") ||
		strings.HasPrefix(lower, "trojan://") ||
		strings.HasPrefix(lower, "ss://") ||
		strings.HasPrefix(lower, "hysteria://") ||
		strings.HasPrefix(lower, "hysteria2://") ||
		strings.HasPrefix(lower, "hy2://") ||
		strings.HasPrefix(lower, "tuic://") ||
		strings.HasPrefix(lower, "socks://") ||
		strings.HasPrefix(lower, "socks4://") ||
		strings.HasPrefix(lower, "socks4a://") ||
		strings.HasPrefix(lower, "socks5://") ||
		strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://")
}

// parsePort converts a decimal port into an integer.
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

// normalizePort converts common JSON port representations to int.
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

// decodeBase64Subscription attempts to decode a complete base64-encoded
// subscription. It deliberately requires recognizable configuration
// content before accepting the result.
func decodeBase64Subscription(value string) (string, bool) {
	value = strings.TrimSpace(value)

	if value == "" {
		return "", false
	}

	decoded, err := decodeBase64Strict(value)

	if err != nil {
		return "", false
	}

	if !looksLikeSubscription(decoded) {
		return "", false
	}

	return decoded, true
}

// looksLikeSubscription checks whether decoded content resembles a
// newline-separated configuration subscription.
func looksLikeSubscription(value string) bool {
	value = strings.TrimSpace(value)

	if value == "" {
		return false
	}

	if strings.Contains(value, "\n") ||
		strings.Contains(value, "\r") {
		return true
	}

	return isSupportedScheme(value)
}

// decodeBase64Strict attempts the common standard, raw and URL-safe
// base64 variants.
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

// formatErrors converts a collection of errors into a compact message.
func formatErrors(errors []error) string {
	messages := make([]string, 0, len(errors))

	for _, err := range errors {
		if err == nil {
			continue
		}

		messages = append(messages, err.Error())
	}

	return strings.Join(messages, "; ")
}
// parseWireGuardURL parses a WireGuard URI.
//
// Supported form:
//
//	wireguard://public-key@host:port?privateKey=...&allowedIPs=0.0.0.0%2F0
//
// The parser also accepts wg:// as an alias.
func parseWireGuardURL(u *url.URL) (config.Config, error) {
	if u.Host == "" {
		return config.Config{}, fmt.Errorf(
			"WireGuard URL has no server address",
		)
	}

	port := parsePort(u.Port())

	if port == 0 {
		return config.Config{}, fmt.Errorf(
			"invalid WireGuard port",
		)
	}

	query := u.Query()

	publicKey := strings.TrimSpace(query.Get("publicKey"))

	if publicKey == "" && u.User != nil {
		publicKey = strings.TrimSpace(u.User.Username())
	}

	if publicKey == "" {
		return config.Config{}, fmt.Errorf(
			"WireGuard URL has no public key",
		)
	}

	privateKey := strings.TrimSpace(query.Get("privateKey"))

	allowedIPs := splitConfigValues(
		query.Get("allowedIPs"),
	)

	if len(allowedIPs) == 0 {
		allowedIPs = splitConfigValues(
			query.Get("allowed_ips"),
		)
	}

	dns := splitConfigValues(query.Get("dns"))

	mtu := parsePort(query.Get("mtu"))

	keepalive := parsePort(
		query.Get("persistentKeepalive"),
	)

	if keepalive == 0 {
		keepalive = parsePort(
			query.Get("persistent_keepalive"),
		)
	}

	return config.Config{
		Type:                config.TypeWireGuard,
		Address:             u.Hostname(),
		Port:                port,
		PublicKey:           publicKey,
		PrivateKey:          privateKey,
		AllowedIPs:          allowedIPs,
		DNS:                 dns,
		MTU:                 mtu,
		PersistentKeepalive: keepalive,
		Name:                u.Fragment,
	}, nil
}

// parseWireGuardConfig parses a standard WireGuard configuration.
//
// The parser intentionally supports the common [Interface] and [Peer]
// fields without attempting to interpret unrelated WireGuard extensions.
func parseWireGuardConfig(text string) (config.Config, error) {
	lines := strings.Split(
		strings.ReplaceAll(text, "\r\n", "\n"),
		"\n",
	)

	section := ""

	values := make(map[string][]string)

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") &&
			strings.HasSuffix(line, "]") {
			section = strings.ToLower(
				strings.TrimSpace(
					line[1 : len(line)-1],
				),
			)
			continue
		}

		if section == "" {
			continue
		}

		key, value, ok := strings.Cut(line, "=")

		if !ok {
			continue
		}

		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		if key == "" || value == "" {
			continue
		}

		values[section+"."+key] = append(
			values[section+"."+key],
			value,
		)
	}

	privateKey := firstConfigValue(
		values,
		"interface.privatekey",
	)

	publicKey := firstConfigValue(
		values,
		"peer.publickey",
	)

	endpoint := firstConfigValue(
		values,
		"peer.endpoint",
	)

	if privateKey == "" {
		return config.Config{}, fmt.Errorf(
			"WireGuard configuration has no private key",
		)
	}

	if publicKey == "" {
		return config.Config{}, fmt.Errorf(
			"WireGuard configuration has no peer public key",
		)
	}

	if endpoint == "" {
		return config.Config{}, fmt.Errorf(
			"WireGuard configuration has no peer endpoint",
		)
	}

	address, port, err := splitEndpoint(endpoint)

	if err != nil {
		return config.Config{}, err
	}

	allowedIPs := splitConfigValues(
		firstConfigValue(
			values,
			"peer.allowedips",
		),
	)

	dns := splitConfigValues(
		firstConfigValue(
			values,
			"interface.dns",
		),
	)

	mtu := parsePort(
		firstConfigValue(
			values,
			"interface.mtu",
		),
	)

	keepalive := parsePort(
		firstConfigValue(
			values,
			"peer.persistentkeepalive",
		),
	)

	return config.Config{
		Type:                config.TypeWireGuard,
		Address:             address,
		Port:                port,
		PublicKey:           publicKey,
		PrivateKey:          privateKey,
		AllowedIPs:          allowedIPs,
		DNS:                 dns,
		MTU:                 mtu,
		PersistentKeepalive: keepalive,
	}, nil
}

func firstConfigValue(
	values map[string][]string,
	key string,
) string {
	items := values[strings.ToLower(key)]

	if len(items) == 0 {
		return ""
	}

	return strings.TrimSpace(items[0])
}

func splitConfigValues(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})

	result := make([]string, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)

		if part == "" {
			continue
		}

		result = append(result, part)
	}

	return result
}

func splitEndpoint(endpoint string) (string, int, error) {
	endpoint = strings.TrimSpace(endpoint)

	if endpoint == "" {
		return "", 0, fmt.Errorf(
			"WireGuard endpoint is empty",
		)
	}

	host, portString, err := net.SplitHostPort(endpoint)

	if err != nil {
		// Some producers omit brackets around IPv6 endpoints.
		// Do not guess an arbitrary port in that case.
		return "", 0, fmt.Errorf(
			"invalid WireGuard endpoint %q: %w",
			endpoint,
			err,
		)
	}

	port := parsePort(portString)

	if port == 0 {
		return "", 0, fmt.Errorf(
			"invalid WireGuard endpoint port %q",
			portString,
		)
	}

	return strings.TrimSpace(host), port, nil
}

func looksLikeWireGuardConfig(text string) bool {
	normalized := strings.ToLower(
		strings.ReplaceAll(text, "\r\n", "\n"),
	)

	return strings.Contains(normalized, "[interface]") &&
		strings.Contains(normalized, "[peer]") &&
		strings.Contains(normalized, "privatekey") &&
		strings.Contains(normalized, "publickey")
}
