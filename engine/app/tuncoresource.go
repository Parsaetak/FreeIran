// tuncoresource.go implements the TUNCoreResolver binding for the
// tunnel package over the EXISTING core infrastructure: the coremgr
// manifest (managed, digest-verified installs). No second manager,
// no second downloader — the TUN dataplane reuses exactly the core
// surface the Cores page and the connection engine already use.
package app

import (
	"context"

	"github.com/Parsaetak/FreeIran/engine/coremgr"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// tunCoreSource resolves the managed sing-box runtime for the TUN
// dataplane. It satisfies tunnel.TUNCoreResolver (engine/app imports
// engine/tunnel; engine/tunnel never imports engine/app — no cycle).
type tunCoreSource struct {
	app *App
}

// SingBoxBinary returns the managed sing-box executable and version.
// The MANAGED manifest is authoritative: the TUN dataplane must run
// from the digest-verified install, never from an arbitrary binary
// found on PATH (the Wintun integrity anchor — docs/security.md).
func (s *tunCoreSource) SingBoxBinary(ctx context.Context) (string, string, error) {
	mf, ok := s.app.coreMgr.Info(coremgr.CoreSingBox)
	if !ok || mf.BinaryPath == "" {
		return "", "", firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "tun-core",
			"managed sing-box core is not installed")
	}

	switch mf.State {
	case coremgr.StateInstalled, coremgr.StateReady, coremgr.StateUpdateAvailable:
		// Usable states: the verified binary is on disk.
	default:
		return "", "", firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "tun-core",
			"managed sing-box core is not usable (state: %s)", mf.State)
	}

	return mf.BinaryPath, mf.Version, nil
}

// EnsureSingBox installs the managed sing-box core through the
// existing digest-verified pipeline when missing.
func (s *tunCoreSource) EnsureSingBox(ctx context.Context) error {
	if _, _, err := s.SingBoxBinary(ctx); err == nil {
		return nil
	}

	return s.app.coreMgr.Install(ctx, coremgr.CoreSingBox)
}
