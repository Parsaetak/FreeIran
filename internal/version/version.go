// Package version provides the single source of truth for the FreeIran
// application version.
//
// The version is kept in one place (VERSION at the repository root) and is
// injected at build time by CI through ldflags. The default value matches
// the VERSION file so local builds never report a stale version.
package version

import "runtime"

// Version is the semantic version of the application.
//
// Build systems override this via:
//
//	-ldflags "-X github.com/Parsaetak/FreeIran/internal/version.Version=x.y.z"
var Version = "0.2.0"

// Commit is the git commit the binary was built from. CI overrides it
// with ldflags; local builds report "dev".
var Commit = "dev"

// UserAgent returns the HTTP user agent used when fetching public
// configuration sources.
func UserAgent() string {
	return "FreeIran/" + Version
}

// String returns a human readable version string including the commit.
func String() string {
	if Commit == "" || Commit == "dev" {
		return Version + " (dev)"
	}

	return Version + " (" + Commit + ", go" + runtime.Version()[2:] + ")"
}
