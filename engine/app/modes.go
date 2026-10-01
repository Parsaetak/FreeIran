// modes.go defines the Quick Connect route modes that survived the
// v0.12.2 provider removal. Tor and Psiphon were removed from the
// active product; the remaining modes map onto the EXISTING
// connection architecture — "auto" is the evidence-based
// ConnectBest flow, "configs" is explicit user selection, and proxy
// chains connect through the same connection state machine.
package app

import "fmt"

// Quick Connect route modes.
const (
	// ConnectModeAuto lets the existing verified-selection engine
	// pick the best candidate (ConnectBest).
	ConnectModeAuto = "auto"

	// ConnectModeConfigs is explicit user selection from the
	// configuration picker.
	ConnectModeConfigs = "configs"

	// ConnectModeChains connects through a user-built proxy chain
	// (same state machine, same verification gate).
	ConnectModeChains = "chains"
)

// AllConnectModes is the UI choice list (order = display order).
var AllConnectModes = []string{
	ConnectModeAuto,
	ConnectModeConfigs,
	ConnectModeChains,
}

// ConnectModeLabel renders a UI label.
func ConnectModeLabel(mode string) string {
	switch mode {
	case ConnectModeAuto:
		return "Auto"
	case ConnectModeConfigs:
		return "Configurations"
	case ConnectModeChains:
		return "Proxy Chains"
	default:
		return "Auto"
	}
}

// MigrateLegacyProviderMode maps a persisted pre-v0.12.2 provider
// mode onto the surviving route modes. Removed Tor/Psiphon choices
// migrate to "auto" — startup must never brick because a removed
// mode is present in settings.json or a profile sidecar.
func MigrateLegacyProviderMode(mode string) string {
	switch mode {
	case ConnectModeConfigs:
		return ConnectModeConfigs
	case ConnectModeChains:
		return ConnectModeChains
	case ConnectModeAuto:
		return ConnectModeAuto
	default:
		// Legacy "tor"/"psiphon"/"provider" values and anything
		// unknown: safe default.
		return ConnectModeAuto
	}
}

// ValidateConnectMode enforces the mode whitelist.
func ValidateConnectMode(mode string) error {
	switch mode {
	case ConnectModeAuto, ConnectModeConfigs, ConnectModeChains, "":
		return nil
	default:
		return fmt.Errorf("unknown connect mode %q", mode)
	}
}
