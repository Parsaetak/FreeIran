package core

import (
	"regexp"
	"strings"
	"sync"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/system"
)

// LogBuffer is a bounded capture buffer for protocol-core output.
//
// Core processes can echo configuration contents — including
// credentials — in their logs. Every write passes through redaction
// (config secret fields + URL credential patterns) BEFORE storage,
// and the buffer keeps only the last maxLines lines, so captured
// output can never grow unbounded or leak secrets.
type LogBuffer struct {
	mu      sync.Mutex
	lines   []string
	max     int
	secrets []string
}

// NewLogBuffer creates a redacting log buffer sized for one core
// launch. Secret material is taken from the configuration that will
// be passed to the core.
func NewLogBuffer(cfg config.Config) *LogBuffer {
	return &LogBuffer{
		lines:   make([]string, 0, defaultLogLines),
		max:     defaultLogLines,
		secrets: cfg.SecretFields(),
	}
}

// defaultLogLines bounds captured output. Cores are chatty on
// startup then quiet; 256 lines is ample for diagnosis.
const defaultLogLines = 256

// Write implements io.Writer: the raw output is redacted and appended
// line-wise to the bounded buffer.
func (b *LogBuffer) Write(p []byte) (int, error) {
	if b == nil || len(p) == 0 {
		return len(p), nil
	}

	redacted := RedactLogText(string(p), b.secrets)

	b.mu.Lock()

	for _, line := range strings.Split(redacted, "\n") {
		line = strings.TrimRight(line, "\r")

		if line == "" {
			continue
		}

		if len(b.lines) >= b.max {
			// Drop the oldest half to amortize the copy.
			drop := b.max / 2
			b.lines = append(b.lines[:0], b.lines[drop:]...)
		}

		b.lines = append(b.lines, line)
	}

	b.mu.Unlock()

	return len(p), nil
}

// Lines returns a copy of the captured (already redacted) lines.
func (b *LogBuffer) Lines() []string {
	if b == nil {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.lines...)
}

// RedactedTail renders the last n captured lines joined for error
// messages and diagnostics.
func (b *LogBuffer) RedactedTail(n int) string {
	lines := b.Lines()

	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	if len(lines) == 0 {
		return "no output captured"
	}

	return strings.Join(lines, " | ")
}

// urlUserinfoPattern matches scheme://credential@ occurrences so the
// credential can be replaced while the host stays visible.
var urlUserinfoPattern = regexp.MustCompile(
	`(?i)\b((?:vless|vmess|trojan|ss|ssr|hysteria|hysteria2|hy2|tuic|socks|https?|wg)://)[^@/\s]+@`,
)

// RedactLogText removes credential material from arbitrary text:
// explicit secret values first, then URL userinfo patterns such as
// vless://uuid@host or trojan://password@host. The result is safe
// for logs, error messages and diagnostics.
func RedactLogText(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}

		text = system.Redact(text, secret)
	}

	return urlUserinfoPattern.ReplaceAllString(text, "$1"+config.SecretPlaceholder+"@")
}
