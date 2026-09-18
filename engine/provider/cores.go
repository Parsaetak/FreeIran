// cores.go adapts the existing protocol cores (Xray, V2Ray,
// sing-box) into the unified provider contract (§10/§13). It is a
// THIN adapter: installation, validation, health, rollback and
// manifests remain owned by engine/coremgr — the single managed-core
// pipeline — so no duplicate install machinery exists. The adapter
// exists so the Cores UI and Auto provider selection can treat every
// provider through one lifecycle vocabulary.
package provider

import (
	"context"
	"time"

	"github.com/Parsaetak/FreeIran/engine/coremgr"
)

// Core licenses and notices (attribution surfaced in the UI).
var coreLicenses = map[coremgr.CoreName]struct{ License, Notice string }{
	coremgr.CoreXray: {
		License: "MIT (Xray-core)",
		Notice:  "Xray-core is developed by XTLS and contributors.",
	},
	coremgr.CoreV2Ray: {
		License: "MIT (V2Ray-core)",
		Notice:  "V2Ray-core is developed by the V2Fly community.",
	},
	coremgr.CoreSingBox: {
		License: "GPL-3.0 (sing-box)",
		Notice:  "sing-box is developed by SagerNet and contributors; distributed unmodified as an independent process.",
	},
}

// CoreProviderAdapter binds one managed core to the Provider
// contract.
type CoreProviderAdapter struct {
	manager *coremgr.Manager
	name    coremgr.CoreName
}

// NewCoreAdapters binds every managed core.
func NewCoreAdapters(manager *coremgr.Manager) []*CoreProviderAdapter {
	if manager == nil {
		return nil
	}

	adapters := make([]*CoreProviderAdapter, 0, len(coremgr.AllCores))

	for _, name := range coremgr.AllCores {
		adapters = append(adapters, &CoreProviderAdapter{manager: manager, name: name})
	}

	return adapters
}

// Name implements Provider.
func (c *CoreProviderAdapter) Name() string { return string(c.name) }

// Kind implements Provider.
func (c *CoreProviderAdapter) Kind() Kind { return KindCore }

// Resolve implements Provider via coremgr's update check.
func (c *CoreProviderAdapter) Resolve(ctx context.Context) (Release, error) {
	info, err := c.manager.CheckForUpdates(ctx, c.name)
	if err != nil {
		return Release{}, err
	}

	return Release{
		Version:    info.LatestVersion,
		AssetURL:   info.AssetURL,
		AssetName:  info.AssetName,
		Size:       info.AssetSize,
		SHA256:     info.AssetSHA256,
		ReleaseURL: info.ReleaseURL,
	}, nil
}

// Install implements Provider (delegates to the managed pipeline).
func (c *CoreProviderAdapter) Install(ctx context.Context) error {
	return c.manager.Install(ctx, c.name)
}

// Uninstall implements Provider.
func (c *CoreProviderAdapter) Uninstall(ctx context.Context) error {
	return c.manager.Remove(ctx, c.name)
}

// Start is NOT a provider-level operation for cores: core processes
// run per node-CONFIGURATION under the connection engine (§11 — the
// existing connection engine stays the source of truth). Calling
// Start returns the honest semantic error.
func (c *CoreProviderAdapter) Start(ctx context.Context) error {
	return errCoreStartUnsupported
}

// Stop mirrors Start.
func (c *CoreProviderAdapter) Stop(ctx context.Context) error {
	return errCoreStartUnsupported
}

var errCoreStartUnsupported = &coreSemanticError{}

type coreSemanticError struct{}

func (*coreSemanticError) Error() string {
	return "protocol cores run per node configuration through the connection engine; provider-level start does not apply"
}

// State implements Provider.
func (c *CoreProviderAdapter) State() LifecycleState {
	manifest, ok := c.manager.Info(c.name)
	if !ok {
		return StateNotInstalled
	}

	switch manifest.State {
	case coremgr.StateNotInstalled:
		return StateNotInstalled
	case coremgr.StateInstalling:
		return StateInstalling
	case coremgr.StateInstalled, coremgr.StateReady:
		return StateInstalled
	case coremgr.StateDisabled:
		return StateDisabled
	case coremgr.StateBroken:
		return StateFailed
	default:
		return StateInstalled
	}
}

// Info implements Provider.
func (c *CoreProviderAdapter) Info() Info {
	manifest, ok := c.manager.Info(c.name)
	if !ok {
		return Info{
			Name:  string(c.name),
			Kind:  KindCore,
			State: StateNotInstalled,
		}
	}

	meta := coreLicenses[c.name]

	info := Info{
		Name:          string(c.name),
		Kind:          KindCore,
		Installed:     manifest.BinaryPath != "",
		Version:       manifest.Version,
		State:         c.State(),
		Source:        manifest.SourceURL,
		License:       meta.License,
		Notice:        meta.Notice,
		LastCheck:     manifest.LastChecked,
		FailureReason: manifest.FailureReason,
	}

	// Capabilities come from what the installed core has
	// demonstrated; a healthy managed core serves node configs.
	if info.Installed && (info.State == StateInstalled) {
		info.Capabilities = []string{"node-configuration-backend"}
	}

	return info
}

// Endpoints implements Provider: cores expose per-connection local
// endpoints owned by the connection engine, not statically.
func (c *CoreProviderAdapter) Endpoints() []Endpoint { return nil }

// Health implements Provider via coremgr's health check.
func (c *CoreProviderAdapter) Health(ctx context.Context) Health {
	result, err := c.manager.HealthCheck(ctx, c.name)

	health := Health{CheckedAt: time.Now().UTC()}

	if err != nil {
		health.Details = err.Error()

		return health
	}

	health.OK = result.OK
	health.Details = result.Details

	if health.OK {
		health.ProcessAlive = true // executable verified + smoke-launched
		health.ListenerReady = true
	}

	return health
}

// Cleanup implements Provider (stale staging cleanup).
func (c *CoreProviderAdapter) Cleanup(ctx context.Context) error {
	c.manager.CleanStaleStaging(ctx, 7*24*time.Hour)

	return nil
}
