package errors

import (
	"regexp"
	"strconv"
	"strings"
)

// portPattern extracts a local port from bind/connect error strings.
var portPattern = regexp.MustCompile(`(?:127\.0\.0\.1|0\.0\.0\.0|:\[?::1\]?):(\d{2,5})`)

// Humanize translates a technical engine error into a first-line,
// user-readable sentence (specification §8: "V2Ray could not start
// because local port 10808 is already in use."). The raw error remains
// available through the technical-details view; this function only
// ever produces the readable layer.
//
// subject is the human name of the component that failed (e.g.
// "V2Ray", "Xray", "The core", "The connection").
func Humanize(err error, subject string) string {
	if err == nil {
		return ""
	}

	if subject == "" {
		subject = "The operation"
	}

	raw := err.Error()
	lower := strings.ToLower(raw)

	switch {
	// Local port already bound by another process — the single most
	// common real-world core startup failure.
	case strings.Contains(lower, "address already in use"),
		strings.Contains(lower, "bind: only one usage of each socket address"),
		strings.Contains(lower, "wsaeaddrinuse"):
		port := extractPort(raw)

		return subject + " could not start because local port " + port +
			" is already in use by another application. Close the application using it (or change the port) and try again."

	case strings.Contains(lower, "permission denied"),
		strings.Contains(lower, "access is denied"):
		return subject + " was blocked by the operating system (permission denied). Check folder permissions or antivirus interference."

	case strings.Contains(lower, "executable"), containsAny(lower, "no such file", "file does not exist", "cannot find the file"):
		return subject + " could not start because its executable is missing or was moved. Reinstall the core from the Cores page."

	case containsAny(lower, "timed out", "timeout", "deadline exceeded"):
		return subject + " did not become ready in time. The endpoint may be unreachable or heavily filtered; try another configuration or backend."

	case containsAny(lower, "connection refused"):
		return subject + " reached the server but it refused the connection. The server may be offline; try another configuration."

	case containsAny(lower, "unreachable", "no route", "network is down"):
		return subject + " could not reach the network. Check the internet connection (Network page) and try again."

	case containsAny(lower, "tls:", "certificate", "x509"):
		return subject + " could not establish a secure connection (TLS failure). The server certificate may be invalid or the connection is being interfered with."

	case containsAny(lower, "authentication", "auth", "password", "uuid", "unauthorized"):
		return subject + " was rejected: the credentials in this configuration are invalid or expired."

	case containsAny(lower, "invalid config", "validate", "configuration"):
		return subject + " rejected the configuration as invalid. Edit the configuration or test it with another backend."

	case containsAny(lower, "cancelled", "canceled", "context canceled"):
		return subject + " was cancelled before it finished."

	default:
		// No known pattern: return a calm sentence with the trimmed
		// technical tail rather than the raw structured error.
		return subject + " failed. Technical details: " + trimTechnical(raw)
	}
}

// extractPort pulls the most plausible local port out of an error.
func extractPort(raw string) string {
	if m := portPattern.FindStringSubmatch(raw); len(m) == 2 {
		return m[1]
	}

	return "the requested"
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}

	return false
}

// trimTechnical shortens kind-prefixed structured errors to their
// human tail.
func trimTechnical(raw string) string {
	// engine/errors formats: "<kind>: <subsystem>/<op>: <detail>"
	if idx := strings.Index(raw, "): "); idx >= 0 && idx < 120 {
		return raw[idx+3:]
	}

	if len(raw) > 240 {
		return raw[:240] + "…"
	}

	return raw
}

// PortNumber is a convenience for callers that already know the port.
func PortNumber(p int) string {
	return strconv.Itoa(p)
}
