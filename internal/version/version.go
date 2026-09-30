// Package version provides the single source of truth for the FreeIran
// application version.
//
// The version is kept in one place (VERSION at the repository root) and is
// injected at build time by CI through ldflags. The default value matches
// the VERSION file so local builds never report a stale version.
//
// v0.11.5 introduced the COMPACT user-facing representation. String()
// renders exactly "v" + Version (e.g. "v0.12.1") — no git commit, no Go runtime
// version, no build tuple. The runtime log and every user-visible surface
// (status bar, diagnostic report, application_start) go through String()
// and therefore stay metadata-free. Build provenance that developer
// machinery genuinely needs continues to flow through the dedicated
// developer diagnostics surface (DeveloperInfo), which reads Commit and
// the Go runtime version directly — never through String().
package version

// Version is the semantic version of the application.
//
// Build systems override this via:
//
//	-ldflags "-X github.com/Parsaetak/FreeIran/internal/version.Version=x.y.z"
var Version = "0.12.1"

// Commit is the git commit the binary was built from. CI overrides it
// with ldflags; local builds report "dev".
//
// This value is developer build provenance. It is exposed ONLY through
// the developer diagnostics surface and is never appended to the
// user-facing version string or the runtime log.
var Commit = "dev"

// SystemIdentity is the digital-system identity FreeIran belongs
// to (v0.9.6): "FreeIran — A SHEYTAN Digital System". The product
// name stays FreeIran; SHEYTAN is the system-level identity, applied
// consistently across the About surface, documentation and release
// metadata.
const SystemIdentity = "SHEYTAN Digital System"

// IdentityLine renders the canonical product identity line.
func IdentityLine() string {
	return "FreeIran — A " + SystemIdentity
}

// UserAgent returns the HTTP user agent used when fetching public
// configuration sources.
func UserAgent() string {
	return "FreeIran/" + Version
}

// Display returns the user-facing version representation: exactly
// "v" + Version. For VERSION=0.12.1 it renders "v0.12.1".
func Display() string {
	return "v" + Version
}

// String returns the compact, user-facing version string. It is an
// alias of Display: metadata-free by contract (v0.11.5).
func String() string {
	return Display()
}
