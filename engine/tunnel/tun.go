// tun.go implements the v0.11.3 TUN backend: the managed sing-box
// core IS the TUN dataplane (native tun inbound, Wintun-backed on
// Windows).
//
// DESIGN CONTRACTS (docs/tun.md):
//
//   - No shell, ever. Routing/DNS/interface work is delegated to
//     sing-box's documented configuration model (auto_route,
//     strict_route, auto_detect_interface, DNS hijack). The v0.9.8.5
//     patterns (netsh/route/PowerShell/curl) never returned.
//   - Wintun integrity: the official sing-box Windows build embeds
//     the official wintun.dll (golang.zx2c4.com/wintun); the anchor
//     is the digest-verified managed sing-box install. FreeIran does
//     not download, extract or PATH-resolve any Wintun binary.
//   - Transactional enable: elevation → verified core → observed
//     interface → observed tunneled request → commit. A failure at
//     any step rolls the session back and surfaces the real error.
//   - Honest state: Active requires the TUN interface to have been
//     OBSERVED (by address, then by exact name) and a real
//     no-explicit-proxy request to have succeeded through the route.
//   - Scoped cleanup/recovery: only the FreeIran-owned adapter name
//     and the recorded session marker are ever inspected; unrelated
//     interfaces, routes and DNS state are never touched.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/singbox"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// tunStatus values surfaced through TUNSnapshot.Status (the UI state
// machine: Off / Starting / Active / Stopping / Failed).
const (
	tunStatusOff      = "off"
	tunStatusStarting = "starting"
	tunStatusActive   = "active"
	tunStatusStopping = "stopping"
	tunStatusFailed   = "failed"
)

// tunBackendName is the honest backend label surfaced to the UI.
const tunBackendName = "sing-box native TUN (Wintun)"

// tunMinCoreVersion is the oldest sing-box release that accepts the
// v0.11.3 TUN document: rule actions arrived in 1.11, the current
// DNS server format in 1.12. Managed installs track current
// releases; a managed core OLDER than this is refused honestly
// instead of generating a config the binary would reject.
const tunMinCoreVersion = "1.12.0"

// TUNCoreResolver supplies the managed sing-box runtime for the TUN
// dataplane. Implemented by the application layer over the EXISTING
// core manager + core registry (never a second manager, never a
// second downloader).
type TUNCoreResolver interface {
	// SingBoxBinary returns the verified sing-box executable path and
	// version, or an error when the managed core is not installed.
	SingBoxBinary(ctx context.Context) (path string, version string, err error)

	// EnsureSingBox installs the managed sing-box core when missing
	// (one-click repair path; runs through coremgr's digest-verified
	// install pipeline).
	EnsureSingBox(ctx context.Context) error
}

// TUNEnableOptions carries everything one TUN activation needs.
type TUNEnableOptions struct {
	// Config is the active/selected configuration routed through the
	// TUN. It must be compatible with the sing-box backend (validated
	// by the caller through the existing compatibility system).
	Config config.Config

	// DiagnosticEndpoint labels the local mixed inbound carried in the
	// TUN document (127.0.0.1:<ephemeral>); used for diagnostics and
	// readiness, never as the enable gate.
	DiagnosticEndpoint string
}

// TUNSessionMarker is the durable crash-recovery record written at
// activation and consumed at verified disable. Its presence at boot
// proves the previous session ended without a clean Disable.
type TUNSessionMarker struct {
	InterfaceName string    `json:"interface_name"`
	IPv4Address   string    `json:"ipv4_address"`
	IPv6Address   string    `json:"ipv6_address,omitempty"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	Configuration string    `json:"configuration"` // redacted display form
}

// tunSessionMarkerPath is the durable marker location (workspace
// runtime dir), configured at boot alongside the system-proxy marker.
var tunSessionMarkerPath string

// SetTUNSessionMarkerPath configures where the TUN session marker is
// persisted. Call once during application boot (app.go) before any
// TUN activation.
func SetTUNSessionMarkerPath(path string) {
	tunSessionMarkerPath = path
}

// StaleTUNSession reports a detected FreeIran-owned TUN session that
// outlived its process (crash, kill, power loss).
type StaleTUNSession struct {
	Found            bool      `json:"found"`
	InterfaceName    string    `json:"interface_name,omitempty"`
	IPv4Address      string    `json:"ipv4_address,omitempty"`
	PID              int       `json:"pid,omitempty"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	InterfacePresent bool      `json:"interface_present"`
	Detail           string    `json:"detail,omitempty"`
}

// CheckStaleTUNSession detects a stale FreeIran-owned TUN state after
// an unclean shutdown. It only ever looks at the RECORDED adapter
// name/address from FreeIran's own marker — unrelated interfaces,
// routes and DNS are never inspected or modified.
//
// Cleanup semantics (honest, bounded): when the stale sing-box
// process is gone, the leftover adapter (if any) belongs to a dead
// session; the next successful elevated TUN enable reuses the
// FreeIranTUN name (or the next free numbered name) and the clean
// Disable removes the newly created adapter. FreeIran never deletes
// adapters or routes it did not create in the same session, and it
// never resets the user's network stack.
func CheckStaleTUNSession() StaleTUNSession {
	if tunSessionMarkerPath == "" {
		return StaleTUNSession{}
	}

	raw, err := os.ReadFile(tunSessionMarkerPath)
	if err != nil {
		return StaleTUNSession{} // no marker: nothing to recover
	}

	var marker TUNSessionMarker
	if err := json.Unmarshal(raw, &marker); err != nil || marker.InterfaceName == "" {
		// A corrupt marker is residue, not evidence — remove it so it
		// cannot shadow future sessions.
		_ = os.Remove(tunSessionMarkerPath)

		return StaleTUNSession{
			Detail: "stale TUN session marker was unreadable and has been cleared",
		}
	}

	session := StaleTUNSession{
		Found:         true,
		InterfaceName: marker.InterfaceName,
		IPv4Address:   marker.IPv4Address,
		PID:           marker.PID,
		StartedAt:     marker.StartedAt,
	}

	// Only the RECORDED FreeIran-owned adapter name is ever inspected
	// (unrelated adapters are never enumerated for action). v0.11.4
	// refines the presence report: name presence decides whether a
	// leftover adapter exists at all; the strict name+address match
	// refines WHAT is reported about it.
	if tunInterfaceNamed(marker.InterfaceName) {
		session.InterfacePresent = true

		if _, exact := findTUNInterface(marker.InterfaceName, marker.IPv4Address); exact {
			session.Detail = "the recorded TUN adapter is still present with its session address; enable TUN once more and disable it (elevated) to finish the cleanup, or remove the adapter manually"
		} else {
			session.Detail = "an adapter with the recorded FreeIran TUN name is still present (without the recorded session address); enable TUN once more and disable it (elevated) to finish the cleanup, or remove the adapter manually"
		}
	} else {
		session.Detail = "the recorded TUN adapter is already gone; only the session marker was left behind"
	}

	logging.LogR(logging.Record{
		Level:     logging.LevelWarn,
		Subsystem: Subsystem,
		Event:     "tun_stale_session",
		Message:   "stale TUN session detected: " + session.Detail,
		Status:    "recovery",
		Fields: map[string]any{
			"interface":         session.InterfaceName,
			"interface_present": session.InterfacePresent,
			"recorded_pid":      session.PID,
		},
	})

	return session
}

func writeTUNSessionMarker(marker TUNSessionMarker) error {
	if tunSessionMarkerPath == "" {
		// Boot wiring missing: refuse to run a session without durable
		// recovery evidence (same posture as the proxy ownership
		// marker).
		return errors.New("tunnel: TUN session marker path is not configured")
	}

	if err := os.MkdirAll(filepath.Dir(tunSessionMarkerPath), 0o700); err != nil {
		return fmt.Errorf("tunnel: TUN session marker dir: %w", err)
	}

	raw, err := json.MarshalIndent(&marker, "", "  ")
	if err != nil {
		return fmt.Errorf("tunnel: encode TUN session marker: %w", err)
	}

	return atomicWriteFile(tunSessionMarkerPath, raw)
}

func clearTUNSessionMarker() error {
	if tunSessionMarkerPath == "" {
		return nil
	}

	err := os.Remove(tunSessionMarkerPath)
	if err != nil && os.IsNotExist(err) {
		return nil
	}

	return err
}

// atomicWriteFile is the write-to-temp → fsync → rename pattern used
// by the recovery markers (never a half-written marker).
func atomicWriteFile(path string, raw []byte) error {
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)

		return err
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)

		return err
	}

	if err := f.Close(); err != nil {
		os.Remove(tmp)

		return err
	}

	return os.Rename(tmp, path)
}

// --- platform-neutral sing-box TUN backend ------------------------------

// singboxTUNBackend runs the TUN dataplane through the managed
// sing-box core. All mutating steps are serialized by mu.
type singboxTUNBackend struct {
	core    TUNCoreResolver
	backend *singbox.Backend

	mu            sync.Mutex
	sessionCtx    context.Context
	sessionCancel context.CancelFunc
	instance      *core.Instance
	status        string
	lastError     string

	// deployed session facts (snapshot authority while active)
	settings   singbox.TUNSettings
	configDesc string
	coreVer    string
	pid        int
	startedAt  time.Time
}

func newSingboxTUNBackend(resolver TUNCoreResolver) TUNBackend {
	return &singboxTUNBackend{
		core:    resolver,
		backend: singbox.New(),
		status:  tunStatusOff,
	}
}

// Available reports whether the platform can serve TUN sessions at
// all (Windows in v0.11.3) and the managed sing-box core resolves.
// It never implies elevation or a running session.
func (b *singboxTUNBackend) Available() bool {
	if !tunPlatformSupported() || b == nil || b.core == nil {
		return false
	}

	_, _, err := b.core.SingBoxBinary(context.Background())

	return err == nil
}

// Install ensures the managed sing-box core exists (the Wintun
// dependency ships embedded in its verified binary). It runs through
// the existing digest-verified install pipeline — never a raw
// download.
func (b *singboxTUNBackend) Install(ctx context.Context) error {
	if b == nil || b.core == nil {
		return fmt.Errorf("tunnel: TUN core resolver is not wired: %w", ErrTunUnavailable)
	}

	return b.core.EnsureSingBox(ctx)
}

// Enable runs the full transactional activation.
func (b *singboxTUNBackend) Enable(ctx context.Context, opts TUNEnableOptions) error {
	b.mu.Lock()

	if b.instance != nil || b.sessionCancel != nil {
		b.mu.Unlock()

		return ErrAlreadyEnabled
	}

	b.mu.Unlock()

	b.setStatus(tunStatusStarting, "")

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "tun_starting",
		Message:   "TUN activation starting: transactional enable (config → platform → elevation → core → session identity → launch → readiness → adapter → route → traffic)",
		Status:    "starting",
	})

	// 1. Config sanity BEFORE anything else.
	if opts.Config.Type == "" {
		return b.failEnable(errors.New("tunnel: enable tun: no active or selected configuration"))
	}

	// 2. Platform honesty (Windows-only in v0.11.3).
	if !tunPlatformSupported() {
		return b.failEnable(fmt.Errorf("tunnel: enable tun: %w", ErrUnsupportedPlatform))
	}

	// 3. Managed, verified sing-box core.
	if b.core == nil {
		return b.failEnable(fmt.Errorf("tunnel: enable tun: TUN core resolver is not wired: %w", ErrTunUnavailable))
	}

	path, version, err := b.core.SingBoxBinary(ctx)
	if err != nil {
		return b.failEnable(fmt.Errorf("tunnel: enable tun: managed sing-box core is not available (install it from the Cores page): %w", err))
	}

	if err := tunCoreVersionOK(version); err != nil {
		return b.failEnable(err)
	}

	// 4. Elevation — checked BEFORE any system mutation. TUN is never
	// silently attempted without administrator privileges.
	if !tunProcessElevated() {
		return b.failEnable(fmt.Errorf("tunnel: enable tun: %w", ErrRequiresElevation))
	}

	// 5. Route context from ACTUAL system state: collision-free TUN
	// addresses and a free adapter name. v0.11.4: the address
	// selection FAILS CLOSED when every IPv4 candidate collides —
	// never the v0.11.3 silent fallback onto an overlapping range.
	v4, v6, err := pickTUNAddresses()
	if err != nil {
		return b.failEnable(fmt.Errorf("tunnel: enable tun: %w", err))
	}

	name, err := pickTUNInterfaceName()
	if err != nil {
		return b.failEnable(fmt.Errorf("tunnel: enable tun: %w", err))
	}

	settings := singbox.TUNSettings{
		InterfaceName: name,
		IPv4Address:   v4,
		IPv6Address:   v6,
		RemoteDNS:     tunRemoteDNS,
		StrictRoute:   true,
	}

	// 6. Launch through the existing supervisor on a SESSION context
	// (Disable owns cancellation — the caller's ctx may end at any
	// time without ending the session).
	sessCtx, cancel := context.WithCancel(context.Background())

	instance, err := b.backend.StartTUN(sessCtx, opts.Config, core.RuntimeOptions{
		BinaryPath:     path,
		BackendVersion: version,
		LocalHost:      "127.0.0.1",
		Purpose:        core.PurposeConnection,
	}, settings)
	if err != nil {
		cancel()

		return b.failEnable(fmt.Errorf("tunnel: enable tun: start sing-box TUN dataplane: %w", err))
	}

	b.mu.Lock()
	b.sessionCtx = sessCtx
	b.sessionCancel = cancel
	b.instance = instance
	b.settings = settings
	b.configDesc = opts.Config.DisplayURL()
	b.coreVer = version
	b.pid = instance.PID()
	b.startedAt = time.Now().UTC()
	b.mu.Unlock()

	// 7. Readiness (the mixed inbound listener) — the shared core
	// launch verdict. A process that exited or never opened its
	// listener fails here with the redacted log tail.
	readyCtx, readyCancel := context.WithTimeout(sessCtx, 45*time.Second)
	defer readyCancel()

	if err := instance.WaitReady(readyCtx); err != nil {
		return b.rollbackEnable(sessCtx, fmt.Errorf("tunnel: enable tun: sing-box readiness: %w", err))
	}

	// 8-9. The activation proof chain: EXACT session adapter →
	// covering route ownership → real tunneled traffic → upstream
	// pinning. v0.11.4 extracts the chain into verifyTUNActivation so
	// every evidence failure mode is unit-testable on every platform.
	if err := verifyTUNActivation(sessCtx, name, v4, instance.PID()); err != nil {
		return b.rollbackEnable(sessCtx, err)
	}

	// 10. Commit: durable session marker FIRST, then state. A crash
	// after this point leaves recoverable evidence.
	if err := writeTUNSessionMarker(TUNSessionMarker{
		InterfaceName: name,
		IPv4Address:   v4,
		IPv6Address:   v6,
		PID:           instance.PID(),
		StartedAt:     b.startedAt,
		Configuration: b.configDesc,
	}); err != nil {
		return b.rollbackEnable(sessCtx, fmt.Errorf("tunnel: enable tun: persist session marker: %w", err))
	}

	b.setStatus(tunStatusActive, "")

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "tun_active",
		Message:   fmt.Sprintf("TUN active: interface %s (%s), core sing-box %s", name, v4, version),
		Status:    "active",
		Fields: map[string]any{
			"interface": name,
			"ipv4":      v4,
			"pid":       instance.PID(),
		},
	})

	return nil
}

// tunInterfaceWaitTimeout / tunInterfaceRemovalWaitTimeout bound the
// activation/removal polls. Variables (not constants) so tests can
// shorten them; production values are generous for slow machines.
var (
	tunInterfaceWaitTimeout        = 20 * time.Second
	tunInterfaceRemovalWaitTimeout = 10 * time.Second
)

// verifyTUNActivation is the v0.11.4 activation proof chain. Each
// step must hold before the next runs, and every failure returns a
// precise error that rolls the session back:
//
//	(a) the EXACT session adapter (name AND expected address on the
//	    same interface) is observed in the live interface table;
//	(b) that adapter OWNS covering IPv4 route state (0.0.0.0/0 or the
//	    0.0.0.0/1 + 128.0.0.0/1 pair) — read from the native OS
//	    forwarding table, never from a process-start signal;
//	(c) a real no-explicit-proxy HTTPS request SUCCEEDS through the
//	    kernel route (the traffic proof);
//	(d) after that traffic, the sing-box process owns NO TCP socket
//	    sourced from the TUN address (the route-loop check); for
//	    TCP-based outbounds the same snapshot yields the POSITIVE
//	    pinned-to-physical evidence (documented honestly for
//	    UDP-family outbounds, where the TCP owner table is silent).
//
// The seams (tunInterfaceSnapshots, collectForwardRoutes,
// collectOwnerTCPSockets, tunProbeInternet) make the whole chain
// deterministic under test; production Windows fills them with the
// native IP Helper collectors.
func verifyTUNActivation(ctx context.Context, name, v4CIDR string, corePID int) error {
	// (a) Exact adapter identity.
	snap, err := waitForTUNInterface(ctx, name, v4CIDR, tunInterfaceWaitTimeout)
	if err != nil {
		return fmt.Errorf(
			"tunnel: enable tun: the exact TUN session adapter (name %q with address %s) was not observed (Wintun creation may have failed): %w",
			name, v4CIDR, err)
	}

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "tun_interface_observed",
		Message:   fmt.Sprintf("TUN session adapter observed: %s (index %d) with %s", name, snap.Index, v4CIDR),
		Status:    "starting",
		Fields: map[string]any{
			"interface": name,
			"ipv4":      v4CIDR,
		},
	})

	// (b) Covering route ownership (native, read-only).
	routes, err := collectForwardRoutes()
	if err != nil {
		return fmt.Errorf("tunnel: enable tun: route observation failed (activation refuses to proceed on unobserved routing state): %w", err)
	}

	verdict := tunRouteCoveringVerdict(routes, uint32(snap.Index))
	if !verdict.Covered {
		return fmt.Errorf("tunnel: enable tun: the TUN adapter does not own the covering IPv4 route state (%s); system traffic would not traverse the TUN", verdict.Detail)
	}

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "tun_route_observed",
		Message:   "TUN route state observed: " + verdict.Detail,
		Status:    "starting",
		Fields: map[string]any{
			"interface": name,
			"covered":   true,
		},
	})

	// (c) Real traffic through the route (no explicit proxy).
	if err := tunProbeInternet(ctx); err != nil {
		return fmt.Errorf("tunnel: enable tun: tunneled Internet request failed (the route is up but traffic does not flow): %w", err)
	}

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "tun_traffic_verified",
		Message:   "tunneled Internet request verified through the TUN route (no explicit proxy)",
		Status:    "starting",
	})

	// (d) Upstream pinning / route-loop check.
	tunIP, err := ipv4OfCIDR(v4CIDR)
	if err != nil {
		return fmt.Errorf("tunnel: enable tun: session address %q is not a valid IPv4 CIDR: %w", v4CIDR, err)
	}

	sockets, err := collectOwnerTCPSockets()
	if err != nil {
		return fmt.Errorf("tunnel: enable tun: upstream socket observation failed (activation refuses to proceed on unobserved loop state): %w", err)
	}

	pinning := tunUpstreamPinningVerdict(sockets, uint32(corePID), tunIP)
	if pinning.Loop {
		return fmt.Errorf("tunnel: enable tun: upstream pinning check FAILED: %s (the route-loop prevention did not hold — the session is rolled back)", pinning.Detail)
	}

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "tun_upstream_observed",
		Message:   "TUN upstream observation: " + pinning.Detail,
		Status:    "starting",
		Fields: map[string]any{
			"loop":   pinning.Loop,
			"pinned": pinning.Pinned,
		},
	})

	return nil
}

// tunProbeInternet is the traffic-proof seam (production: the clean
// no-explicit-proxy HTTPS probe below; tests: deterministic fakes).
var tunProbeInternet = probeTunneledInternet

// ipv4OfCIDR extracts the address bytes of an IPv4 CIDR string.
func ipv4OfCIDR(cidr string) ([4]byte, error) {
	addr := cidr
	if i := strings.Index(addr, "/"); i >= 0 {
		addr = addr[:i]
	}

	ip := net.ParseIP(addr)
	if ip == nil {
		return [4]byte{}, fmt.Errorf("%q is not an IP address", addr)
	}

	v4 := ip.To4()
	if v4 == nil {
		return [4]byte{}, fmt.Errorf("%q is not an IPv4 address", addr)
	}

	return [4]byte{v4[0], v4[1], v4[2], v4[3]}, nil
}

// failEnable records the failure honestly (status=failed, error kept
// for the snapshot) and returns it. No partial state survives.
func (b *singboxTUNBackend) failEnable(err error) error {
	b.setStatus(tunStatusFailed, err.Error())

	return err
}

// rollbackEnable unwinds a failed activation: stop the process,
// cancel the session context, verify the TUN interface is gone, and
// surface a composed error. Any residual (interface still present
// after the process is confirmed dead) is REPORTED, never silenced.
func (b *singboxTUNBackend) rollbackEnable(sessCtx context.Context, cause error) error {
	composed := cause

	if b.sessionCancel != nil {
		b.sessionCancel()
	}

	b.mu.Lock()
	instance := b.instance
	name := b.settings.InterfaceName
	b.mu.Unlock()

	if instance != nil {
		if err := instance.Close(); err != nil {
			composed = fmt.Errorf("%w (dataplane stop also failed: %v)", cause, err)
		}
	}

	if name != "" {
		if residual := waitForTUNInterfaceRemoved(sessCtx, name, tunInterfaceRemovalWaitTimeout); residual {
			composed = fmt.Errorf("%w (RESIDUAL: the TUN adapter %q is still present after teardown — it will be reported as stale TUN state)", cause, name)

			logging.LogR(logging.Record{
				Level:     logging.LevelWarn,
				Subsystem: Subsystem,
				Event:     "tun_residual",
				Message:   fmt.Sprintf("TUN rollback residual: adapter %q is still present after teardown", name),
				Status:    "failed",
				Fields:    map[string]any{"interface": name},
			})
		}
	}

	b.mu.Lock()
	b.sessionCtx = nil
	b.sessionCancel = nil
	b.instance = nil
	b.settings = singbox.TUNSettings{}
	b.configDesc = ""
	b.pid = 0
	b.mu.Unlock()

	return b.failEnable(composed)
}

// Disable tears the TUN session down transactionally: stop the
// dataplane → wait for exit → verify the interface is gone → consume
// the session marker → report. A failed verification is surfaced (a
// residual marker is kept so the next boot reports the stale state).
func (b *singboxTUNBackend) Disable(ctx context.Context) error {
	b.mu.Lock()

	if b.instance == nil {
		b.mu.Unlock()

		b.setStatus(tunStatusOff, "")

		return nil
	}

	b.setStatus(tunStatusStopping, "")

	instance := b.instance
	name := b.settings.InterfaceName
	b.mu.Unlock()

	stopErr := instance.Close()

	if b.sessionCancel != nil {
		b.sessionCancel()
	}

	residual := waitForTUNInterfaceRemoved(ctx, name, tunInterfaceRemovalWaitTimeout)

	if residual {
		logging.LogR(logging.Record{
			Level:     logging.LevelWarn,
			Subsystem: Subsystem,
			Event:     "tun_residual",
			Message:   fmt.Sprintf("TUN disable residual: adapter %q is still present after the dataplane stopped", name),
			Status:    "failed",
			Fields:    map[string]any{"interface": name},
		})
	}

	var composed error

	switch {
	case residual && stopErr != nil:
		composed = fmt.Errorf("tunnel: disable tun: RESIDUAL: TUN adapter %q is still present after the dataplane stopped (stop error: %v; routes may remain until the next elevated TUN cycle)", name, stopErr)
	case residual:
		composed = fmt.Errorf("tunnel: disable tun: RESIDUAL: TUN adapter %q is still present after the dataplane stopped (routes may remain until the next elevated TUN cycle)", name)
	case stopErr != nil:
		composed = fmt.Errorf("tunnel: disable tun: %w", stopErr)
	}

	b.mu.Lock()
	b.sessionCtx = nil
	b.sessionCancel = nil
	b.instance = nil
	b.settings = singbox.TUNSettings{}
	b.configDesc = ""
	b.pid = 0
	b.mu.Unlock()

	if composed != nil {
		b.setStatus(tunStatusFailed, composed.Error())

		return composed
	}

	if err := clearTUNSessionMarker(); err != nil {
		err = fmt.Errorf("tunnel: disable tun: session marker cleanup failed: %w", err)
		b.setStatus(tunStatusFailed, err.Error())

		return err
	}

	b.setStatus(tunStatusOff, "")

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "tun_stop",
		Message:   fmt.Sprintf("TUN disabled: interface %s removed", name),
		Status:    "off",
	})

	return nil
}

// Snapshot returns the live, honest TUN state. Interface and address
// fields carry OBSERVED values while active; DNS/routes describe the
// deployed sing-box design (they are sing-box-managed kernel state
// that FreeIran deliberately does not scrape — see docs/tun.md).
func (b *singboxTUNBackend) Snapshot() TUNSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	snap := TUNSnapshot{
		Available:         tunPlatformSupported(),
		Installed:         b.coreAvailable(),
		RequiresElevation: tunPlatformSupported() && !tunProcessElevated(),
		Backend:           tunBackendName,
		Status:            b.status,
		Core:              b.coreVer,
		Configuration:     b.configDesc,
		Details:           b.lastError,
	}

	if b.instance != nil {
		snap.InterfaceName = b.settings.InterfaceName
		snap.IPv4Address = b.settings.IPv4Address
		snap.IPv6Address = b.settings.IPv6Address
		snap.DNSServers = tunDNSDescription(b.settings.RemoteDNS)
		snap.Routes = tunRouteDescription(b.settings.InterfaceName)
		snap.Active = b.status == tunStatusActive

		// Re-verify liveness each time the snapshot is read: a crashed
		// dataplane downgrades the status honestly.
		if snap.Active && b.instance.State() != core.StateRunning {
			snap.Active = false
			snap.Status = tunStatusFailed
			snap.Details = "the sing-box TUN process is no longer running (state: " + string(b.instance.State()) + ")"
		}
	}

	return snap
}

func (b *singboxTUNBackend) coreAvailable() bool {
	if b.core == nil {
		return false
	}

	_, _, err := b.core.SingBoxBinary(context.Background())

	return err == nil
}

func (b *singboxTUNBackend) setStatus(status, lastError string) {
	b.mu.Lock()
	b.status = status
	b.lastError = lastError
	b.mu.Unlock()
}

// --- system-state helpers (observed, never hardcoded) -------------------

// tunRemoteDNS is the proxied DoH resolver used for hijacked client
// DNS. A public encrypted resolver reached THROUGH the tunnel; it is
// a routing choice, not a hardcode of any user-specific fact.
const tunRemoteDNS = "1.1.1.1"

// tunIPv4Candidates / tunIPv6Candidates are the TUN address
// candidates tried in order against the LIVE interface table; the
// first range that does not overlap any existing local network wins.
// 172.19.0.1/30 is the sing-box documentation default; the rest are
// documented-use ranges unlikely to collide with consumer LANs.
var (
	tunIPv4Candidates = []string{
		"172.19.0.1/30",
		"172.19.0.5/30",
		"172.19.0.9/30",
		"172.16.159.1/30",
		"172.16.203.1/30",
		"10.207.0.1/30",
	}

	tunIPv6Candidates = []string{
		"fdfe:dcba:9876::1/126",
		"fdf1:2ec4:7e31::1/126",
		"fdf1:2ec4:7e32::1/126",
	}
)

// tunInterfaceSnapshot is one observed interface with its address
// strings (CIDR or bare form). The normalized shape the identity
// checks reason over — small, cheap to fake, and identical on every
// platform.
type tunInterfaceSnapshot struct {
	Name  string
	Index int
	Addrs []string
}

// liveTUNInterfaceSnapshot observes the REAL interface table through
// the Go net stack (net.Interfaces + Addrs — native Windows
// GetAdaptersAddresses underneath; no shell, no netsh). It is the
// production seam value; tests replace tunInterfaceSnapshots with
// deterministic fakes to exercise collision and identity cases
// without mutating the user's network.
func liveTUNInterfaceSnapshot() ([]tunInterfaceSnapshot, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	out := make([]tunInterfaceSnapshot, 0, len(ifaces))

	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue // an interface whose addresses cannot be read is skipped, not fatal
		}

		snap := tunInterfaceSnapshot{Name: ifc.Name, Index: ifc.Index}

		for _, addr := range addrs {
			snap.Addrs = append(snap.Addrs, addr.String())
		}

		out = append(out, snap)
	}

	return out, nil
}

// tunInterfaceSnapshots is the interface-table observation seam.
var tunInterfaceSnapshots = liveTUNInterfaceSnapshot

// tunExistingLocalPrefixes is the live-prefix observation seam used
// by pickTUNAddresses (tests inject deterministic collision tables).
var tunExistingLocalPrefixes = existingLocalPrefixes

// existingLocalPrefixes enumerates the prefixes of every live
// interface address (the route context is built from real system
// state).
func existingLocalPrefixes() []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, 16)

	ifaces, err := net.Interfaces()
	if err != nil {
		return prefixes
	}

	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipn, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}

			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}

			bits, _ := ipn.Mask.Size()
			prefixes = append(prefixes, netip.PrefixFrom(ip.Unmap(), bits))
		}
	}

	return prefixes
}

// pickTUNAddresses selects TUN interface addresses that do not
// overlap the user's existing local networks. IPv6 is best-effort:
// when every candidate collides (or none parses) the session runs
// IPv4-only and says so. IPv4 is NOT best-effort (v0.11.4): when
// every IPv4 candidate collides with a live prefix the selection
// FAILS CLOSED — knowingly deploying the TUN onto an overlapping
// subnet is exactly the silent misroute the v0.11.3 fallback
// allowed, and it is never acceptable. The caller surfaces the error
// before any system mutation happens.
func pickTUNAddresses() (v4 string, v6 string, err error) {
	existing := tunExistingLocalPrefixes()

	free := func(cidr string) bool {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return false
		}

		for _, ex := range existing {
			if ex.Overlaps(p) {
				return false
			}
		}

		return true
	}

	for _, candidate := range tunIPv4Candidates {
		if free(candidate) {
			v4 = candidate

			break
		}
	}

	if v4 == "" {
		// No collision-free candidate: FAIL CLOSED. The enable is
		// refused before the core is launched — never silently
		// downgraded onto an overlapping range.
		return "", "", fmt.Errorf(
			"no collision-free IPv4 TUN address exists on this host (all %d candidates overlap a live interface prefix); "+
				"enable refused to avoid routing onto an existing network",
			len(tunIPv4Candidates))
	}

	for _, candidate := range tunIPv6Candidates {
		if free(candidate) {
			v6 = candidate

			break
		}
	}

	return v4, v6, nil
}

// pickTUNInterfaceName returns a free adapter name: FreeIranTUN, or
// the first free numbered variant. Exact-name ambiguity with a stale
// adapter is avoided by construction.
func pickTUNInterfaceName() (string, error) {
	snapshots, err := tunInterfaceSnapshots()
	if err != nil {
		return "", fmt.Errorf("enumerate interfaces: %w", err)
	}

	names := make(map[string]bool, len(snapshots))
	for _, snap := range snapshots {
		names[snap.Name] = true
	}

	base := "FreeIranTUN"
	if !names[base] {
		return base, nil
	}

	for i := 2; i <= 8; i++ {
		candidate := base + strconv.Itoa(i)
		if !names[candidate] {
			return candidate, nil
		}
	}

	return "", errors.New("no free TUN adapter name (FreeIranTUN..FreeIranTUN8 all exist)")
}

// findTUNInterface observes the live interface table for the EXACT
// session adapter. v0.11.4 invariant (the v0.11.3 false-positive
// repair): the observed interface must match the recorded FreeIran
// adapter identity AND carry the session's expected address ON THAT
// SAME INTERFACE. Neither
//   - the expected address present on a DIFFERENT adapter, nor
//   - an adapter with the right name but WITHOUT the expected
//     address
//
// is accepted — both were accepted by the v0.11.3 address-first
// scan. The activation gate identifies the exact session adapter.
func findTUNInterface(name, v4CIDR string) (tunInterfaceSnapshot, bool) {
	wantIP := v4CIDR
	if i := strings.Index(wantIP, "/"); i >= 0 {
		wantIP = wantIP[:i]
	}

	snapshots, err := tunInterfaceSnapshots()
	if err != nil {
		return tunInterfaceSnapshot{}, false
	}

	for _, snap := range snapshots {
		if snap.Name != name {
			continue // exact adapter identity first
		}

		for _, addr := range snap.Addrs {
			ipPart := addr
			if i := strings.Index(ipPart, "/"); i >= 0 {
				ipPart = ipPart[:i]
			}

			if wantIP != "" && ipPart == wantIP {
				return snap, true
			}
		}
	}

	return tunInterfaceSnapshot{}, false
}

// tunInterfaceNamed reports whether ANY adapter carries the recorded
// FreeIran-owned name. Used by teardown verification: the name is
// collision-free by construction (pickTUNInterfaceName), so an
// adapter with that name after the dataplane stopped is a residual —
// whether or not the session address is still attached to it.
func tunInterfaceNamed(name string) bool {
	if name == "" {
		return false
	}

	snapshots, err := tunInterfaceSnapshots()
	if err != nil {
		return false
	}

	for _, snap := range snapshots {
		if snap.Name == name {
			return true
		}
	}

	return false
}

// waitForTUNInterface polls the live interface table until the EXACT
// session adapter (name + expected address) appears, or the deadline
// passes. The returned snapshot carries the adapter's interface
// index for the route observation that follows.
func waitForTUNInterface(ctx context.Context, name, v4CIDR string, timeout time.Duration) (tunInterfaceSnapshot, error) {
	deadline := time.Now().Add(timeout)

	for {
		if snap, ok := findTUNInterface(name, v4CIDR); ok {
			return snap, nil
		}

		if time.Now().After(deadline) {
			return tunInterfaceSnapshot{}, errors.New("timeout waiting for the TUN interface")
		}

		sleep := time.NewTimer(250 * time.Millisecond)

		select {
		case <-ctx.Done():
			sleep.Stop()

			return tunInterfaceSnapshot{}, ctx.Err()
		case <-sleep.C:
		}
	}
}

// waitForTUNInterfaceRemoved polls until no adapter with the
// FreeIran-owned NAME remains, and reports whether a residual adapter
// was still present at the deadline.
func waitForTUNInterfaceRemoved(ctx context.Context, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for {
		if !tunInterfaceNamed(name) {
			return false
		}

		if time.Now().After(deadline) {
			return true
		}

		sleep := time.NewTimer(250 * time.Millisecond)

		select {
		case <-ctx.Done():
			sleep.Stop()

			return true
		case <-sleep.C:
		}
	}
}

// probeTunneledInternet verifies a REAL request reaches the Internet
// through the TUN: a clean transport (NO environment/system proxy, no
// interface binding) riding the kernel route. This is the difference
// between "the sing-box process started" and "the tunnel works".
func probeTunneledInternet(ctx context.Context) error {
	client := &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil, // NEVER route the probe through an explicit proxy
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			DialContext:           (&net.Dialer{Timeout: 8 * time.Second}).DialContext,
		},
	}
	defer client.CloseIdleConnections()

	// Same independent 204 targets the connection verification uses.
	targets := []string{
		"https://www.gstatic.com/generate_204",
		"https://cp.cloudflare.com/generate_204",
	}

	var lastErr error

	for _, target := range targets {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err

			continue
		}

		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
			return nil
		}

		lastErr = fmt.Errorf("probe target %s returned status %d", target, resp.StatusCode)
	}

	if lastErr == nil {
		lastErr = errors.New("no probe target attempted")
	}

	return lastErr
}

// tunCoreVersionOK guards the TUN document against cores too old to
// parse it. Unknown version strings are allowed through — the runtime
// readiness + config check will produce the honest failure.
func tunCoreVersionOK(version string) error {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return nil // unknown format: let the runtime decide
	}

	major, errM := strconv.Atoi(parts[0])
	minor, errN := strconv.Atoi(parts[1])
	if errM != nil || errN != nil {
		return nil
	}

	if major < 1 || (major == 1 && minor < 12) {
		return firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "enable-tun",
			"managed sing-box %s is too old for the TUN dataplane (requires %s+); update the core from the Cores page",
			version, tunMinCoreVersion)
	}

	return nil
}

// tunDNSDescription renders the deployed DNS design for the snapshot.
func tunDNSDescription(remote string) []string {
	return []string{
		fmt.Sprintf("hijacked plain DNS → https://%s/dns-query (DoH via proxy)", remote),
		"local (bootstrap resolution of the proxy server only)",
	}
}

// tunRouteDescription renders the deployed routing design.
func tunRouteDescription(iface string) []string {
	return []string{
		"auto_route: all traffic → " + iface,
		"strict_route: on (multihomed DNS leak prevention)",
		"auto_detect_interface: proxy upstream bound to the physical interface (loop prevention)",
		"ip_is_private → direct (local networks stay local)",
	}
}
