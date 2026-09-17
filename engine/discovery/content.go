package discovery

import (
	"regexp"
	"strings"
)

// content.go implements source-content discovery (v0.9.6 §5, level
// 6): valid fetched content often REFERENCES other configuration
// sources — READMEs link sibling subscriptions, aggregators credit
// the upstream projects they merge. Discovering those references
// extends the source graph from evidence inside already-trusted
// content, which is both higher-signal than blind Internet scraping
// and bounded by construction (only references inside content that
// already parsed successfully are considered).
//
// Only raw endpoints are ever accepted (raw.githubusercontent.com,
// the rare direct .txt/.yaml hosts); HTML pages are deliberately
// ignored so the engine never scrapes rendered HTML.

// contentRefPattern matches absolute HTTPS URLs inside plain text
// that plausibly point at subscription-style configuration files.
// The extension constraint (.txt/.md/.yaml/.yml/.json/.conf) keeps
// the extracted set small; validation happens later when the URL is
// actually fetched and parsed (a reference is a LEAD, not a source).
var contentRefPattern = regexp.MustCompile(
	`https://[A-Za-z0-9.\-]+/[A-Za-z0-9._\-/~%]+?\.(?:txt|md|yaml|yml|json|conf)\b`,
)

// rawGitHubPattern recognizes raw.githubusercontent.com URLs, which
// are the canonical raw endpoints for GitHub-hosted content.
var rawGitHubPattern = regexp.MustCompile(
	`^https://raw\.githubusercontent\.com/[A-Za-z0-9.\-_]+/[A-Za-z0-9.\-_]+/[A-Za-z0-9.\-_]+/.+`,
)

// MaxContentRefs caps how many references one content body may
// contribute — a README that links 500 files is a directory, not a
// lead, and the discovery budget is better spent elsewhere.
const MaxContentRefs = 8

// ContentRefs extracts bounded, deduplicated raw-endpoint references
// from source content. HTML blob URLs and non-raw GitHub pages are
// excluded by construction.
func ContentRefs(content []byte) []string {
	if len(content) == 0 {
		return nil
	}

	seen := make(map[string]struct{})

	var out []string

	for _, match := range contentRefPattern.FindAllString(string(content), -1) {
		ref := strings.TrimSpace(match)

		if !isPlausibleRawEndpoint(ref) {
			continue
		}

		if _, dup := seen[ref]; dup {
			continue
		}

		seen[ref] = struct{}{}
		out = append(out, ref)

		if len(out) >= MaxContentRefs {
			break
		}
	}

	return out
}

// isPlausibleRawEndpoint accepts raw.githubusercontent.com URLs and
// direct file URLs on other hosts. It deliberately rejects github
// HTML pages (/blob/, /blame/, /tree/): those return wrapper HTML,
// not raw configuration content.
func isPlausibleRawEndpoint(u string) bool {
	if rawGitHubPattern.MatchString(u) {
		return true
	}

	lower := strings.ToLower(u)

	if strings.Contains(lower, "github.com/") {
		// Only the raw host is accepted; anything else on github.com
		// is an HTML page.
		return false
	}

	return strings.HasPrefix(lower, "https://")
}

// SearchTermsForProtocol returns the search-term fragments for a
// protocol, used by the smart search to build queries appropriate to
// the configuration formats the parser supports.
func SearchTermsForProtocol(protocol string) []string {
	switch strings.ToLower(protocol) {
	case "vless":
		return []string{"vless", "reality", "vless config"}
	case "vmess":
		return []string{"vmess", "vmess subscription"}
	case "trojan":
		return []string{"trojan", "trojan config"}
	case "ss", "shadowsocks":
		return []string{"shadowsocks", "ss subscription"}
	case "hysteria2":
		return []string{"hysteria2", "hysteria config"}
	case "tuic":
		return []string{"tuic", "tuic config"}
	case "wireguard":
		return []string{"wireguard", "wg conf"}
	default:
		return []string{"proxy config"}
	}
}
