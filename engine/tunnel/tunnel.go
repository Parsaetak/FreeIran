// Package tunnel implements FreeIran's user-facing tunnel modes:
//
//   - System Proxy: sets Windows system proxy through WinINet's
//     per-connection options, with safe save/restore of the previous
//     settings. PRODUCTION-supported.
//   - TUN mode: DISABLED in this release. The v0.9.8.6 audit removed
//     the unfinished Wintun backend (inverted route tracking,
//     non-transactional DNS "restore", unverified curl/PowerShell
//     acquisition, unbounded extraction) — see tun_unavailable.go
//     for the full defect list. TUN is NOT a kill switch and must
//     never be described as one; re-enabling it requires a
//     transactional implementation with verified rollback.
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
	"sort"
	"strings"
	"sync"
	"time"
)

// Subsystem identifies the tunnel layer in structured errors.
const Subsystem = "tunnel"

// WinINet INTERNET_PER_CONN_OPTION tags and PROXY_TYPE_* flag bits
// (documented in proxy_windows.go; declared here because the
// platform-neutral snapshot algebra compares them).
const (
	internetPerConnFlags              = 1
	internetPerConnProxyServer        = 2
	internetPerConnProxyBypass        = 3
	internetPerConnAutoconfigURL      = 4
	internetPerConnAutoDiscoveryFlags = 5

	proxyTypeDirect       = 1
	proxyTypeProxy        = 2
	proxyTypeAutoProxyURL = 4
	proxyTypeAutoDetect   = 8

	// allProxyTypeFlags masks the four proxy bits of
	// INTERNET_PER_CONN_FLAGS for faithful capture/restore.
	allProxyTypeFlags = proxyTypeDirect | proxyTypeProxy |
		proxyTypeAutoProxyURL | proxyTypeAutoDetect
)

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

	// ModeTUN: DISABLED (v0.9.8.6). TUN remains in the mode enum so
	// the UI can report it as unavailable/experimental, but Enable
	// always returns ErrTunExperimental until a safe, transactional
	// implementation exists. See tun_unavailable.go.
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

	// Current reads the ACTUAL platform proxy state right now
	// (v0.10.2). The transactional ownership contract (recovery.go)
	// verifies the machine state before and after activation and
	// during boot recovery — in-memory bookkeeping is never accepted
	// as evidence. Implementations that cannot read the platform
	// state return ErrUnsupportedPlatform.
	Current() (SystemProxySnapshot, error)

	// Restore applies a previously persisted proxy state (crash
	// recovery). The v0.10.1 recovery path (RecoverStaleProxy)
	// replays the state recorded before a crashed session took
	// ownership: exactly what Disable would have restored, applied
	// without any in-memory context. Implementations that cannot
	// mutate the platform state return ErrUnsupportedPlatform.
	Restore(previous SystemProxySnapshot) error
}

// SystemProxySnapshot is a redacted view of the system proxy state.
type SystemProxySnapshot struct {
	Enabled  bool     `json:"enabled"`
	Server   string   `json:"server,omitempty"`
	Bypass   []string `json:"bypass,omitempty"`
	Override string   `json:"override,omitempty"` // raw override string from WinINet
	Saved    bool     `json:"saved"`              // previous settings saved?

	// v0.10.2 fidelity fields. The v0.10.1 marker (recovery record)
	// did not persist these, so legacy markers parse with zero
	// values and recovery derives the mode from Enabled/Server —
	// documented as the v1 fidelity limit.
	//
	//   AutoConfigURL — the PAC/autoconfig URL (INTERNET_PER_CONN_AUTOCONFIG_URL)
	//   AutoDetect    — the PROXY_TYPE_AUTO_DETECT flag bit
	//   Flags         — the raw INTERNET_PER_CONN_FLAGS value (diagnostics)
	AutoConfigURL string `json:"autoconfig_url,omitempty"`
	AutoDetect    bool   `json:"autodetect,omitempty"`
	Flags         uint32 `json:"flags,omitempty"`

	// v0.10.3 fidelity field: the connection's autodiscovery
	// settings (INTERNET_PER_CONN_AUTODISCOVERY_FLAGS, option 5 —
	// the AUTO_PROXY_FLAG_* mask that Windows keeps BESIDE the
	// PROXY_TYPE_AUTO_DETECT flag bit). v0.10.2 never captured or
	// restored it, so a session could not faithfully restore the
	// WPAD autodetection state it had saved: the AUTO_PROXY flags
	// live in this option, not in INTERNET_PER_CONN_FLAGS, and
	// Windows re-derives the AUTO_DETECT flag bit from it on query.
	// Zero = not captured (legacy v1 records) — the comparison
	// algebra treats zero as "unknown", never as "absent".
	AutoDiscoveryFlags uint32 `json:"autodiscovery_flags,omitempty"`
}

// ErrOwnershipResidual is returned when the proxy STATE was restored
// but the durable ownership marker could not be consumed (v0.10.2).
// The invariant is "never silently claim a clean outcome while
// ownership residue remains": the caller must surface this error and
// the next boot will retry the restoration (which is idempotent — it
// re-applies exactly the state Disable just applied).
var ErrOwnershipResidual = errors.New("tunnel: system-proxy ownership marker residue remains")

// TUNBackend is the platform interface for TUN operations. The only
// implementation in this release is unavailableTUNBackend (see
// tun_unavailable.go): TUN is experimental and disabled until a
// transactional design exists.
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

// --- platform-neutral snapshot algebra (v0.10.2) ---
//
// The transactional ownership contract compares the ACTUAL platform
// state against the state a record claims. The comparison must be
// normalized (whitespace, bypass ordering) and must work for legacy
// (v1) snapshots that carry no Flags/AutoConfigURL fields.

// proxyModeOf derives the canonical proxy mode from a snapshot,
// preferring the explicit flags when present and falling back to the
// legacy Enabled/Server pair for v1 records.
func proxyModeOf(s SystemProxySnapshot) string {
	if s.Flags != 0 {
		switch {
		case s.Flags&proxyTypeAutoProxyURL != 0:
			return "autoconfig"
		case s.Flags&proxyTypeProxy != 0:
			return "explicit"
		case s.Flags&proxyTypeAutoDetect != 0:
			return "autodetect"
		default:
			return "direct"
		}
	}

	// Legacy derivation (v0.10.1 snapshots): explicit proxy when
	// Enabled + a server string; PAC/autodetect were not captured.
	if s.Enabled && s.Server != "" {
		return "explicit"
	}

	return "direct"
}

// isLegacySnapshot reports whether a snapshot predates the v0.10.2
// fidelity fields (no raw flags captured). The comparison algebra
// must not hold a legacy snapshot to fields it never recorded: the
// documented v1 fidelity limit is that PAC/autodetect state was not
// captured, so a legacy-vs-captured comparison verifies only the
// fields BOTH sides actually describe.
func isLegacySnapshot(s SystemProxySnapshot) bool {
	return s.Flags == 0
}

// normalizedProxyState returns a canonical copy: trimmed strings,
// sorted bypass entries, legacy fields folded into the mode.
func normalizedProxyState(s SystemProxySnapshot) SystemProxySnapshot {
	out := SystemProxySnapshot{
		Server:             strings.TrimSpace(s.Server),
		AutoConfigURL:      strings.TrimSpace(s.AutoConfigURL),
		AutoDetect:         s.AutoDetect,
		AutoDiscoveryFlags: s.AutoDiscoveryFlags,
	}

	if len(s.Bypass) > 0 {
		out.Bypass = make([]string, 0, len(s.Bypass))
		for _, entry := range s.Bypass {
			entry = strings.TrimSpace(entry)
			if entry != "" {
				out.Bypass = append(out.Bypass, entry)
			}
		}
		sort.Strings(out.Bypass)
	}

	out.Flags = s.Flags & (proxyTypeDirect | proxyTypeProxy | proxyTypeAutoProxyURL | proxyTypeAutoDetect)
	if out.Server != "" && out.Flags == 0 && proxyModeOf(s) == "explicit" {
		// keep legacy semantics visible to comparison via Enabled
		out.Enabled = true
	}

	return out
}

// proxyStatesEqual compares two snapshots in normalized form: the
// canonical MODE must match, then server, PAC URL, autodetect and the
// bypass set (order-insensitive).
//
// v0.10.3 comparison semantics (documented, not weakened blindly):
//
//   - The canonical MODE always compares — it is the effective proxy
//     behavior and both legacy and captured snapshots derive it.
//   - Server and the bypass set always compare — they are the
//     effective explicit-proxy configuration.
//   - The PAC URL compares only when at least one side claims the
//     autoconfig MODE: Windows PRESERVES a stored-but-inactive
//     AutoConfigURL in the connection settings after the flags move
//     away from PROXY_TYPE_AUTO_PROXY_URL, so an inactive stale URL
//     is not part of the effective state and cannot fail a restore
//     that never promised to clear it. When the mode IS autoconfig,
//     the URL is the effective configuration and compares strictly.
//   - AutoDetect compares only when at least one side is a CAPTURED
//     snapshot (raw flags present). A legacy v1 snapshot never
//     recorded autodetect state (the documented v1 fidelity limit);
//     holding it to an observed AUTO_DETECT bit would fail recovery
//     for state it could not have known.
//   - AutoDiscoveryFlags compares only when the EXPECTED side
//     recorded it (non-zero): zero means "not captured" for legacy
//     records, never "the machine must show none".
func proxyStatesEqual(a, b SystemProxySnapshot) bool {
	na, nb := normalizedProxyState(a), normalizedProxyState(b)

	if proxyModeOf(na) != proxyModeOf(nb) {
		return false
	}

	if na.Server != nb.Server {
		return false
	}

	modeA, modeB := proxyModeOf(na), proxyModeOf(nb)

	if (modeA == "autoconfig" || modeB == "autoconfig") && na.AutoConfigURL != nb.AutoConfigURL {
		return false
	}

	if !isLegacySnapshot(na) || !isLegacySnapshot(nb) {
		if na.AutoDetect != nb.AutoDetect {
			return false
		}
	}

	if na.AutoDiscoveryFlags != 0 && na.AutoDiscoveryFlags != nb.AutoDiscoveryFlags {
		return false
	}

	if len(na.Bypass) != len(nb.Bypass) {
		return false
	}

	for i := range na.Bypass {
		if na.Bypass[i] != nb.Bypass[i] {
			return false
		}
	}

	return true
}

// dumpProxyState renders a snapshot for FAILURE DIAGNOSTICS: every
// fidelity field, the canonical mode and the raw flag mask. This is
// the §16 CI diagnostic contract — a verification mismatch must
// print BOTH sides in full so the exact divergent field is visible
// in the Actions log (flags, mode, server, bypass, PAC, autodetect).
func dumpProxyState(label string, s SystemProxySnapshot) string {
	bypass := "<none>"

	if len(s.Bypass) > 0 {
		bypass = "[" + strings.Join(s.Bypass, "; ") + "]"
	}

	return fmt.Sprintf("%s: mode=%s flags=0x%x autodiscovery=0x%x autodetect=%t server=%q bypass=%s pac=%q override=%q",
		label, proxyModeOf(s), s.Flags, s.AutoDiscoveryFlags, s.AutoDetect,
		s.Server, bypass, s.AutoConfigURL, s.Override)
}

// systemProxyActivated reports whether the observed platform state
// proves FreeIran's explicit proxy at host:port is live. This is the
// activation verification of the transactional ownership contract —
// TCP reachability is NOT enough; the WinINet state itself must show
// the explicit proxy.
func systemProxyActivated(observed SystemProxySnapshot, host string, port int) bool {
	if observed.Flags != 0 {
		if observed.Flags&proxyTypeProxy == 0 {
			return false
		}
	} else if !observed.Enabled {
		return false
	}

	endpoint := fmt.Sprintf("%s:%d", host, port)

	// The server string may be scheme-prefixed ("socks=...") or a
	// multi-scheme list; the endpoint must appear as a component.
	for _, part := range strings.Split(observed.Server, ";") {
		part = strings.TrimSpace(part)
		if _, after, ok := strings.Cut(part, "="); ok {
			part = strings.TrimSpace(after)
		}
		if part == endpoint {
			return true
		}
	}

	return false
}

// New constructs a Controller with the platform-default backends.
// On Windows, systemProxy wraps WinINet and tun wraps Wintun; on
// other platforms both are no-op stubs that return
// ErrUnsupportedPlatform.
func New() *Controller {
	return NewWithProxyBackend(newSystemProxyBackend())
}

// CaptureSystemProxySnapshot reads the ACTUAL platform proxy state
// through the platform backend (WinINet on Windows). It exists for
// the guaranteed-restore-path contract: a test (or diagnostic tool)
// that may let the application mutate the machine's proxy settings
// must FIRST capture this snapshot and MUST defer
// RestoreSystemProxySnapshot with a verified restore. On platforms
// without a platform backend the error says so — never a fake
// snapshot.
func CaptureSystemProxySnapshot() (SystemProxySnapshot, error) {
	return newSystemProxyBackend().Current()
}

// RestoreSystemProxySnapshot applies a previously captured snapshot
// back to the platform and VERIFIES the platform state matches
// before reporting success — the same gate boot recovery uses. This
// is the other half of the guaranteed restore path.
func RestoreSystemProxySnapshot(s SystemProxySnapshot) error {
	return newSystemProxyBackend().Restore(s)
}

// NewWithProxyBackend constructs a Controller with an injected
// system-proxy backend. The tun backend stays the platform default
// (TUN is disabled everywhere in this release). The injection point
// exists for the recovery contract tests, which must prove the
// marker lifecycle without depending on WinINet.
func NewWithProxyBackend(sp SystemProxyBackend) *Controller {
	return &Controller{
		systemProxy: sp,
		tun:         newTUNBackend(),
		state: State{
			Mode: ModeDirect,
		},
	}
}

// Enable activates one tunnel mode against the given endpoint.
//
// For ModeSystemProxy the endpoint is a SOCKS5 (or HTTP) listener on
// the active core. v0.10.2 makes the ownership TRANSACTIONAL
// (recovery.go § "transactional ownership"):
//
//	capture previous state
//	→ durably persist the validated recovery record
//	→ activate FreeIran proxy
//	→ verify the resulting platform state
//	→ mark ownership ACTIVE
//
// so the invariant "FreeIran changed the system proxy ⇒ durable
// ownership evidence already exists" holds at every instant. On any
// activation or verification failure the previous state is restored
// (best-effort) and the marker is consumed; a rollback that cannot
// clean up keeps the marker and reports the residual explicitly.
//
// For ModeTUN the function returns ErrTunExperimental: TUN is
// disabled in this release (v0.9.8.6) because its implementation was
// not transactional and its Wintun acquisition was not verifiable.
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
		endpoint := fmt.Sprintf("%s:%d", host, port)

		// 1. Capture the ACTUAL platform state (never in-memory
		// bookkeeping) as the state a crash recovery must restore.
		previous, err := c.systemProxy.Current()
		if err != nil {
			return fmt.Errorf("tunnel: enable system proxy: capture current platform state: %w", err)
		}

		// 2. Durably persist the validated recovery record BEFORE the
		// proxy is touched. A crash anywhere after this point leaves a
		// restorable workspace. A persistence failure must abort the
		// activation: enabling without durable ownership evidence is
		// exactly the defect class v0.10.1 shipped.
		if err := writeRecoveryMarker(previous, endpoint); err != nil {
			return fmt.Errorf("tunnel: enable system proxy: persist ownership evidence: %w", err)
		}

		// 3. Activate the FreeIran proxy.
		if err := c.systemProxy.Enable(ctx, host, port, opts.AsHTTP, opts.Bypass); err != nil {
			return c.rollbackEnable(previous, fmt.Errorf("tunnel: enable system proxy: %w", err))
		}

		// 4. Verify the resulting WinINet state actually shows the new
		// explicit proxy. A state that cannot be verified did not
		// (verifiably) happen.
		observed, err := c.systemProxy.Current()
		if err != nil {
			return c.rollbackEnable(previous,
				fmt.Errorf("tunnel: enable system proxy: verify activation: %w", err))
		}

		if !systemProxyActivated(observed, host, port) {
			return c.rollbackEnable(previous,
				fmt.Errorf("tunnel: enable system proxy: verification failed: platform state does not show the activated proxy"))
		}

		// 5. Mark ownership ACTIVE. The durable evidence already exists
		// (step 2), so a failure here only downgrades the record's
		// phase label — recovery treats pending and active alike and
		// the residual is logged, never silent.
		if err := markOwnershipActive(endpoint); err != nil {
			markerLog("marker_active_update_failed", err)
		}

		c.state = State{
			Mode:       mode,
			Active:     true,
			Endpoint:   endpoint,
			BypassList: opts.Bypass,
			StartedAt:  time.Now().UTC(),
		}
		c.enabled = true
		return nil

	case ModeTUN:
		// v0.9.8.6: TUN is experimental and disabled everywhere (see
		// tun_unavailable.go). Fail with the explicit, user-visible
		// status BEFORE touching any platform backend — never fake
		// support, never half-configure the system.
		return fmt.Errorf("tunnel: enable tun: %w", ErrTunExperimental)

	default:
		return fmt.Errorf("tunnel: unknown mode %q", mode)
	}
}

// rollbackEnable unwinds a failed system-proxy activation: the
// previous state is restored (best-effort — a restore failure keeps
// the marker so the next boot retries, which is safe because the
// record holds exactly the state being restored), the marker is
// consumed on success, and the composed error is returned.
func (c *Controller) rollbackEnable(previous SystemProxySnapshot, cause error) error {
	if restoreErr := c.systemProxy.Restore(previous); restoreErr != nil {
		markerLog("rollback_restore_failed", restoreErr)

		return fmt.Errorf("%w (rollback restore also failed: %v; ownership marker kept for next-boot recovery)", cause, restoreErr)
	}

	if err := clearRecoveryMarker(); err != nil {
		markerLog("rollback_marker_cleanup_failed", err)

		return fmt.Errorf("%w (rollback restored the previous state but %v)", cause, err)
	}

	return cause
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

		// The proxy ownership ended cleanly: the crash-recovery
		// marker must not outlive the session that wrote it.
		//
		// v0.10.2: a marker that cannot be removed is an explicit
		// residual error, never a silent success — the next boot
		// would otherwise "recover" a state that is already exactly
		// current (harmless, but the ownership residue must be
		// visible to the user and to diagnostics).
		if err := clearRecoveryMarker(); err != nil {
			c.state = State{
				Mode:    ModeDirect,
				Details: "system proxy restored, but ownership marker cleanup failed: " + err.Error(),
			}
			c.enabled = false

			return fmt.Errorf("tunnel: disable system proxy: %w", ErrOwnershipResidual)
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
