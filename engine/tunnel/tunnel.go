// Package tunnel implements FreeIran's user-facing tunnel modes:
//
//   - System Proxy: sets Windows system proxy through WinINet's
//     per-connection options, with safe save/restore of the previous
//     settings.
//   - TUN mode: a real Windows TUN interface backed by Wintun (not a
//     fake system-proxy substitution).
//
// Both modes require explicit user action and run independently of
// the protocol-core execution boundary (engine/core). They consume
// the SOCKS/HTTP endpoint exposed by the active core Instance.
//
// Platform code is isolated behind build tags so the rest of the
// engine stays OS-neutral.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Subsystem identifies the tunnel layer in structured errors.
const Subsystem = "tunnel"

// Mode is the user-facing tunnel mode.
type Mode string

const (
	// ModeDirect: no tunnel. All traffic leaves the system as-is.
	ModeDirect Mode = "direct"

	// ModeSystemProxy: Windows system proxy is set to the local
	// SOCKS/HTTP endpoint exposed by the active core. Browsers and
	// most user apps honour the system proxy. No kernel-level
	// routing.
	ModeSystemProxy Mode = "system_proxy"

	// ModeTUN: a real TUN interface is created (Wintun on Windows),
	// routing is configured to send traffic through the TUN, and the
	// active core's SOCKS endpoint is used as the upstream. Requires
	// elevation on Windows.
	ModeTUN Mode = "tun"
)

// State is the current tunnel state surfaced to the UI.
type State struct {
	Mode              Mode      `json:"mode"`
	Active            bool      `json:"active"`
	Endpoint          string    `json:"endpoint,omitempty"`
	Backend           string    `json:"backend,omitempty"`
	BypassList        []string  `json:"bypass_list,omitempty"`
	SavedProxy        string    `json:"saved_proxy,omitempty"` // redacted (no credentials)
	SavedProxyEnable  bool      `json:"saved_proxy_enable,omitempty"`
	RequiresElevation bool      `json:"requires_elevation,omitempty"`
	StartedAt         time.Time `json:"started_at,omitempty"`
	Details           string    `json:"details,omitempty"`
}

// Controller owns the active tunnel mode. It is safe for concurrent
// use: Enable/Disable acquire a per-instance lock so the UI cannot
// race itself into an inconsistent state.
type Controller struct {
	mu      sync.Mutex
	state   State
	enabled bool

	// systemProxy implements the platform-specific proxy operations.
	// On Windows it wraps WinINet; on other platforms it is a no-op
	// that returns ErrUnsupportedPlatform.
	systemProxy SystemProxyBackend

	// tun implements the platform-specific TUN operations.
	tun TUNBackend
}

// SystemProxyBackend is the platform interface for system-proxy
// operations. Implementations MUST save the previous settings on
// Enable and restore them on Disable.
type SystemProxyBackend interface {
	// Enable sets the system proxy to socks5://host:port (or
	// http://host:port) with the given bypass list. The previous
	// settings are saved internally so Disable can restore them.
	Enable(ctx context.Context, host string, port int, asHTTP bool, bypass []string) error

	// Disable restores the previous system-proxy settings.
	Disable(ctx context.Context) error

	// Snapshot returns the current system-proxy state (for the UI).
	Snapshot() SystemProxySnapshot
}

// SystemProxySnapshot is a redacted view of the system proxy state.
type SystemProxySnapshot struct {
	Enabled  bool     `json:"enabled"`
	Server   string   `json:"server,omitempty"`
	Bypass   []string `json:"bypass,omitempty"`
	Override string   `json:"override,omitempty"` // raw override string from WinINet
	Saved    bool     `json:"saved"`              // previous settings saved?
}

// TUNBackend is the platform interface for TUN operations.
type TUNBackend interface {
	// Available reports whether the TUN driver (Wintun.dll) is
	// installed and usable.
	Available() bool

	// Install installs the Wintun driver if missing. Returns nil if
	// already installed. Requires elevation on Windows.
	Install(ctx context.Context) error

	// Enable creates the TUN interface, configures routes + DNS, and
	// starts forwarding packets to the local SOCKS endpoint. Requires
	// elevation on Windows.
	Enable(ctx context.Context, host string, port int) error

	// Disable tears down the TUN interface and restores routing.
	Disable(ctx context.Context) error

	// Snapshot returns the current TUN state.
	Snapshot() TUNSnapshot
}

// TUNSnapshot is a redacted view of the TUN state.
type TUNSnapshot struct {
	Available         bool     `json:"available"`
	Installed         bool     `json:"installed"`
	InterfaceName     string   `json:"interface_name,omitempty"`
	IPv4Address       string   `json:"ipv4_address,omitempty"`
	IPv6Address       string   `json:"ipv6_address,omitempty"`
	DNSServers        []string `json:"dns_servers,omitempty"`
	Routes            []string `json:"routes,omitempty"`
	RequiresElevation bool     `json:"requires_elevation"`
}

// ErrUnsupportedPlatform is returned when a mode is not supported on
// the current OS (e.g. TUN on non-Windows).
var ErrUnsupportedPlatform = errors.New("tunnel: mode not supported on this platform")

// ErrAlreadyEnabled is returned when Enable is called while enabled.
var ErrAlreadyEnabled = errors.New("tunnel: already enabled")

// ErrNotEnabled is returned when Disable is called while disabled.
var ErrNotEnabled = errors.New("tunnel: not enabled")

// ErrRequiresElevation is returned when TUN Enable is called without
// administrator privileges.
var ErrRequiresElevation = errors.New("tunnel: TUN mode requires administrator privileges")

// New constructs a Controller with the platform-default backends.
// On Windows, systemProxy wraps WinINet and tun wraps Wintun; on
// other platforms both are no-op stubs that return
// ErrUnsupportedPlatform.
func New() *Controller {
	return &Controller{
		systemProxy: newSystemProxyBackend(),
		tun:         newTUNBackend(),
		state: State{
			Mode: ModeDirect,
		},
	}
}

// Enable activates one tunnel mode against the given endpoint.
//
// For ModeSystemProxy the endpoint is a SOCKS5 (or HTTP) listener on
// the active core. The function saves the previous system-proxy
// settings, sets the new proxy with the bypass list, and updates
// State. On failure the previous settings are NOT modified (the
// function returns before any state mutation).
//
// For ModeTUN the endpoint is upstream of the TUN interface. The
// function checks Wintun availability, requires elevation, creates
// the TUN, configures routes + DNS, and forwards packets. On failure
// all changes are rolled back (route + interface removed).
//
// For ModeDirect the function is a no-op (use Disable instead).
func (c *Controller) Enable(ctx context.Context, mode Mode, host string, port int, opts Options) error {
	if mode == ModeDirect {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.enabled {
		return ErrAlreadyEnabled
	}

	switch mode {
	case ModeSystemProxy:
		if err := c.systemProxy.Enable(ctx, host, port, opts.AsHTTP, opts.Bypass); err != nil {
			return fmt.Errorf("tunnel: enable system proxy: %w", err)
		}
		c.state = State{
			Mode:       mode,
			Active:     true,
			Endpoint:   fmt.Sprintf("%s:%d", host, port),
			BypassList: opts.Bypass,
			StartedAt:  time.Now().UTC(),
		}
		c.enabled = true
		return nil

	case ModeTUN:
		if !c.tun.Available() {
			if err := c.tun.Install(ctx); err != nil {
				return fmt.Errorf("tunnel: install wintun: %w", err)
			}
		}
		if err := c.tun.Enable(ctx, host, port); err != nil {
			return fmt.Errorf("tunnel: enable tun: %w", err)
		}
		c.state = State{
			Mode:              mode,
			Active:            true,
			Endpoint:          fmt.Sprintf("%s:%d", host, port),
			RequiresElevation: true,
			StartedAt:         time.Now().UTC(),
		}
		c.enabled = true
		return nil

	default:
		return fmt.Errorf("tunnel: unknown mode %q", mode)
	}
}

// Disable deactivates the active tunnel mode and restores the
// previous settings (system proxy) or tears down the TUN interface.
// Idempotent: safe to call when not enabled.
func (c *Controller) Disable(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled {
		return nil
	}

	switch c.state.Mode {
	case ModeSystemProxy:
		if err := c.systemProxy.Disable(ctx); err != nil {
			return fmt.Errorf("tunnel: disable system proxy: %w", err)
		}
	case ModeTUN:
		if err := c.tun.Disable(ctx); err != nil {
			return fmt.Errorf("tunnel: disable tun: %w", err)
		}
	}

	c.state = State{Mode: ModeDirect}
	c.enabled = false
	return nil
}

// State returns the current tunnel state.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()

	snap := c.state
	switch snap.Mode {
	case ModeSystemProxy:
		s := c.systemProxy.Snapshot()
		snap.SavedProxy = s.Server
		snap.SavedProxyEnable = s.Saved
	case ModeTUN:
		t := c.tun.Snapshot()
		snap.RequiresElevation = t.RequiresElevation
	}
	return snap
}

// Options configures one Enable call.
type Options struct {
	// AsHTTP: when true, set the HTTP proxy; when false, set the
	// SOCKS proxy. Some Windows apps honour HTTP proxy only.
	AsHTTP bool

	// Bypass is the list of hostnames / IP ranges that should
	// bypass the proxy (e.g. "localhost", "127.0.0.1", "10.0.0.0/8",
	// "*.local").
	Bypass []string
}
