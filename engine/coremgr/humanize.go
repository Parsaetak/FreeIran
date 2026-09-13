package coremgr

import (
	"fmt"
	"runtime"
	"strings"
)

// HumanizeInstallFailure translates an install pipeline failure into
// a user-readable sentence. The raw error is still available in the
// log and through the technical-details view; this is the "why did my
// install fail" line the UI shows first.
func HumanizeInstallFailure(stage string, err error) string {
	if err == nil {
		return ""
	}

	raw := err.Error()
	lower := strings.ToLower(raw)

	switch stage {
	case "resolve_release":
		switch {
		case strings.Contains(lower, "http"):
			return "Could not reach the " + upstreamName(raw) + " release server. Check the internet connection and try again."
		case strings.Contains(lower, "no asset"):
			return "No download is published for this platform in the latest release. The upstream project may not support it yet."
		default:
			return "Could not determine the latest release of the core. " + technicalHint(raw)
		}
	case "select_asset":
		return "The latest release does not publish a build for this platform (" + platformString() + ")."
	case "download":
		switch {
		case strings.Contains(lower, "http 4"):
			return "The core download was rejected by the server (HTTP client error). The release may have been pulled; retry later."
		case strings.Contains(lower, "http 5"):
			return "The core download failed because the server reported an error. Retry in a few minutes."
		case strings.Contains(lower, "context deadline"), strings.Contains(lower, "timeout"):
			return "The core download timed out. A slow or blocked connection can cause this; retry, preferably on a stable link."
		default:
			return "The core download failed. " + technicalHint(raw)
		}
	case "verify_digest":
		return "The downloaded core failed its SHA-256 verification and was discarded. This can indicate a corrupted or tampered download; the healthy core was left untouched."
	case "hash":
		return "The downloaded core could not be hashed locally (disk error?). Free space and permissions were checked automatically on retry."
	case "unpack":
		return "The downloaded archive could not be unpacked. The download was most likely corrupted; retry the install."
	case "locate_executable":
		return "The archive did not contain the expected core executable. The upstream package layout may have changed."
	case "probe_version":
		if strings.Contains(lower, "reports version") {
			return strings.ToUpper(raw[:1]) + raw[1:] + "."
		}

		return "The downloaded core did not report a usable version and was rejected before activation."
	case "validate_executable":
		return "The downloaded core rejected a valid minimal configuration, so it cannot be trusted to run. It was not activated; the previous core is untouched."
	case "activate":
		return "The verified core could not be moved into place (file locked by another process?). Close running instances and retry."
	case "mkdir_bin", "mkdir_staging", "mkdir_unpacked", "reset_staging":
		return "The cores directory is not writable. Check disk space and folder permissions."
	default:
		return "The core installation failed. " + technicalHint(raw)
	}
}

// HumanizeHealthFailure translates a failed HealthResult into a
// user-readable sentence describing which stage of the smoke test
// failed and what that means.
func HumanizeHealthFailure(result HealthResult) string {
	switch {
	case !result.ExecutableExists:
		return "The core executable is missing from its installation directory. Reinstall the core to restore it."
	case !result.VersionQuery:
		return "The core executable exists but does not answer a version query — it may be corrupted or blocked by antivirus. Reinstall the core."
	case !result.ConfigValidate:
		return "The core starts but rejects a valid configuration. The installed build may be incompatible; reinstall or roll back."
	case !result.SmokeLaunch:
		if portIssue(result.Details) {
			return "The core could not complete its smoke test because a local port is already in use by another application."
		}

		return "The core did not become ready during its smoke test. Check the technical details for the captured output."
	case !result.CleanShutdown:
		return "The core ran but could not be stopped deterministically. A leftover process may need to be closed manually."
	default:
		return "The core failed its last verification."
	}
}

// portIssue reports whether a failure detail looks like a local port
// conflict — one of the most common real-world core failures.
func portIssue(detail string) bool {
	lower := strings.ToLower(detail)

	return strings.Contains(lower, "bind") ||
		strings.Contains(lower, "address already in use") ||
		strings.Contains(lower, "port")
}

// upstreamName extracts the repo name from a GitHub API error string
// so failure messages can name the project that was unreachable.
func upstreamName(raw string) string {
	for _, marker := range []string{"github.com/repos/", "github.com/"} {
		if idx := strings.Index(raw, marker); idx >= 0 {
			rest := raw[idx+len(marker):]
			if slash := strings.Index(rest, "/"); slash > 0 {
				return rest[:slash]
			}

			return rest
		}
	}

	return "upstream"
}

// platformString renders the current platform for messages.
func platformString() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// technicalHint appends the raw error as a parenthetical.
func technicalHint(raw string) string {
	return fmt.Sprintf("(technical details: %.240s)", raw)
}

// ExtractVersionToken pulls the loose-semver-looking token out of a
// version line such as:
//
//	Xray 26.3.27 (Xray, Penetrates Everything.)
//	V2Ray 5.53.0 (V2Fly, a V2Ray community.)
//	sing-box version 1.14.0 (go1.26 linux/amd64)
//	fakecore v0-test
//
// It returns the first token that looks like "v?<digits>(.<digits>)*"
// with at least one numeric component. Empty when no token matches.
func ExtractVersionToken(line string) string {
	line = strings.TrimSpace(line)

	for _, token := range strings.FieldsFunc(line, versionTokenSplit) {
		token = strings.TrimPrefix(strings.TrimPrefix(token, "v"), "V")

		if token == "" {
			continue
		}

		if looksNumeric(token) {
			return token
		}
	}

	return ""
}

// versionTokenSplit splits on anything that cannot appear inside a
// version number.
func versionTokenSplit(r rune) bool {
	switch {
	case r >= '0' && r <= '9':
		return false
	case r == '.':
		return false
	case r >= 'a' && r <= 'z':
		return false
	case r >= 'A' && r <= 'Z':
		return false
	default:
		return true
	}
}

// looksNumeric reports whether the token contains at least one digit
// and only digits/dots otherwise.
func looksNumeric(token string) bool {
	digits := 0

	for _, r := range token {
		if r >= '0' && r <= '9' {
			digits++

			continue
		}

		if r != '.' {
			return false
		}
	}

	return digits > 0
}
