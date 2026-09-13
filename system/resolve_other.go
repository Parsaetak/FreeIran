//go:build !windows

package system

import (
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// resolveExecutable validates the ProcessSpec.Path with exactly the
// v0.7 semantics on Unix-like platforms: the path must point to an
// existing regular file (bare names therefore fail unless they exist
// relative to the working directory, which has always been the
// documented contract — protocol cores are always launched through
// explicit, registry-resolved paths).
func resolveExecutable(path string) (string, error) {
	if !fileExists(path) {
		return "", firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "start", "core binary %q", path)
	}

	return path, nil
}

// ResolveSystemExecutable has no bare-name system binaries on the
// Unix launcher contract; tests use explicit paths (/bin/sh and
// friends are resolved by the tests themselves).
func ResolveSystemExecutable(name string) (string, error) {
	return "", firerrors.New(firerrors.KindDependencyUnavailable,
		Subsystem, "start",
		"bare executable name %q is not a known system binary; "+
			"provide an explicit path", name)
}
