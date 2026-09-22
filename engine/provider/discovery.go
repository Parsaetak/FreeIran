package provider

// v0.9.14 — automatic discovery and reuse of installed provider
// engines (Tor, Psiphon). One discovery authority (the shared
// system.CoreLocator / ExecDiscovery) is consulted; a working local
// engine is ADOPTED — Tor by reference (external ownership, the file
// is never touched), Psiphon through the existing copy-not-move
// content-addressed user-binary adoption path. No provider downloads
// a duplicate bundle while a working installation is already present.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Parsaetak/FreeIran/system"
)

// discoveryTimeout bounds one engine-discovery attempt (the locator's
// version probes carry their own per-form timeouts; the overall bound
// keeps EnsureAvailable predictable on the interactive path).
const discoveryTimeout = 20 * time.Second

// SetDiscovery attaches the shared executable-discovery authority.
// Optional: without it the engines keep their pure download semantics
// (deterministic tests, headless harnesses).
func (e *TorEngine) SetDiscovery(locator *system.CoreLocator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.discovery = locator
}

// SetDiscovery attaches the shared executable-discovery authority
// (Psiphon counterpart).
func (e *PsiphonEngine) SetDiscovery(locator *system.CoreLocator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.discovery = locator
}

// adoptInstalledTor discovers an already-installed Tor and, when one
// validates, records an EXTERNAL reference in the provider manifest.
// The discovered binary is neither copied, moved, renamed nor deleted;
// FreeIran only records where it lives and what it reported.
//
// Returns the adopted engine version, or "" when no suitable
// installation exists.
func (e *TorEngine) adoptInstalledTor(ctx context.Context) (string, error) {
	e.mu.Lock()
	locator := e.discovery
	e.mu.Unlock()

	if locator == nil {
		return "", nil
	}

	discCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	candidates := locator.DiscoverCandidates(discCtx, TorName)

	for _, cand := range candidates {
		if !cand.Validated() {
			continue // no version evidence — not a candidate
		}

		// The SAME validation and smoke path a downloaded bundle must
		// pass — never bypass the engine's own checks.
		version, err := validateTorBinary(discCtx, cand.Path)
		if err != nil || version == "" {
			continue
		}

		// Capability floor: the runtime requires webtunnel-capable
		// tor (>= 0.4.8); an older system tor is not a viable
		// candidate for this provider.
		if !torSupportsWebTunnel(version) {
			continue
		}

		if err := smokeTestTor(discCtx, cand.Path); err != nil {
			continue
		}

		manifest := Manifest{
			Name:         TorName,
			Version:      version,
			BinaryPath:   cand.Path,
			SourceURL:    "local:" + string(cand.Origin),
			InstalledAt:  time.Now().UTC(),
			LastChecked:  time.Now().UTC(),
			State:        string(StateInstalled),
			Ownership:    string(OwnershipExternal),
			Origin:       string(cand.Origin),
			ExternalPath: cand.Path,
		}

		if err := e.binary.saveManifest(manifest); err != nil {
			return "", fmt.Errorf("persist external tor reference: %w", err)
		}

		logInstall(TorName, "provider_reused", map[string]any{
			"origin":  string(cand.Origin),
			"version": version,
			"mode":    "external-reference",
		})

		return version, nil
	}

	return "", nil
}

// EnsureAvailable reports whether a usable Tor engine exists without
// downloading anything: the exact managed bundle (checksum match), or
// a working installed Tor adopted by reference. It NEVER acquires —
// automatic discovery must not become automatic software updating.
func (e *TorEngine) EnsureAvailable(ctx context.Context) bool {
	// A checksum comparison needs the latest release metadata; without
	// network the managed install is still reusable when the manifest
	// and binary agree it is installed and healthy.
	manifest := e.binary.LoadManifest()

	if manifest.BinaryPath != "" {
		if _, err := os.Stat(manifest.BinaryPath); err == nil {
			if manifest.State == string(StateInstalled) || manifest.State == string(StateReady) {
				return true
			}
		}
	}

	// Discovery + adoption: an installed Tor satisfies availability.
	if version, err := e.adoptInstalledTor(ctx); err == nil && version != "" {
		return true
	}

	return false
}

// EnsureAvailable reports whether a usable Psiphon engine exists
// without downloading anything. An auto-discovered consoleclient is
// adopted through the EXISTING user-binary path (copy-not-move,
// content-addressed, full validation + smoke) — the same honest
// local-validation semantics as a manual adoption; the official
// release channel's missing digests are never fabricated.
func (e *PsiphonEngine) EnsureAvailable(ctx context.Context) bool {
	manifest := e.binary.LoadManifest()

	if manifest.BinaryPath != "" {
		if _, err := os.Stat(manifest.BinaryPath); err == nil {
			if manifest.State == string(StateInstalled) || manifest.State == string(StateReady) {
				return true
			}
		}
	}

	e.mu.Lock()
	locator := e.discovery
	e.mu.Unlock()

	if locator == nil {
		return false
	}

	discCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	candidates := locator.DiscoverCandidates(discCtx, PsiphonName)

	for _, cand := range candidates {
		if !cand.Validated() {
			continue
		}

		// Adoption through the existing, idempotent user-binary path:
		// copy-not-move, content-addressed managed name, validation
		// and smoke test. The source file remains untouched.
		if err := e.SetUserBinary(ctx, cand.Path); err == nil {
			logInstall(PsiphonName, "provider_reused", map[string]any{
				"origin": string(cand.Origin),
				"mode":   "user-binary-adoption",
			})

			return true
		}
	}

	return false
}
