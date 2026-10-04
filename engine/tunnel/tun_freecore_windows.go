//go:build windows

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/freecore"
	fctun "github.com/Parsaetak/FreeIran/engine/freecore/tun"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// tunObserve is the OS observation seam (swappable in tests: the
// activation gate must be testable against synthetic interface/route
// facts without an elevated physical adapter).
var tunObserve = fctun.Observe

// freecoreTUNBackend is the FIRST-PARTY TUN dataplane (v0.14.0):
// FreeIran-owned Wintun device → FreeIran userspace IP stack → FreeIran
// engine sessions → Router → first-party outbound. It reuses the ONE
// TunnelService authority (it IS a TUNBackend implementation, not a new
// service), the deterministic adapter identity, and the transactional
// rollback model of the activation contract. Every OS mutation goes
// through freecore/tun.ApplyInterfaceConfig and is recorded as an undo.
type freecoreTUNBackend struct {
	mu      sync.Mutex
	status  string // off | starting | active | stopping | failed
	details string

	cancel     context.CancelFunc
	device     fctun.Device
	dataplane  *freecore.TUNDataplane
	rollback   []func() error
	identity   fctun.Identity
	ipv4       netip.Prefix
	ipv6       netip.Prefix
	startedAt  time.Time
	configured string
	evidence   tunEvidence
}

// newFreecoreTUNBackend builds the first-party backend (Windows).
func newFreecoreTUNBackend() TUNBackend {
	return &freecoreTUNBackend{status: tunStatusOff}
}

// Available reports the platform gate: Windows + a compiled-in
// first-party dataplane.
func (b *freecoreTUNBackend) Available() bool {
	return true
}

// Install: the first-party dataplane has no external dependency set.
func (b *freecoreTUNBackend) Install(_ context.Context) error { return nil }

// freecoreBackend is injected by the platform-neutral wiring (one
// capability oracle).
func (b *freecoreTUNBackend) setStatus(status, details string) {
	b.mu.Lock()
	b.status = status
	b.details = details
	b.mu.Unlock()
}

// Enable runs the transactional first-party activation:
//
//	validate first-party route → elevation → collision-free identity →
//	open Wintun → configure interface/routes (recorded undos) → start
//	engine + userspace stack → verify adapter + route ownership (both
//	families) + physical upstream → publish Active.
//
// On any failure: stop stack, cancel flows, close device, undo exactly
// the FreeIran-owned mutations, surface residual failures honestly.
func (b *freecoreTUNBackend) Enable(ctx context.Context, opts TUNEnableOptions) error {
	b.mu.Lock()
	if b.device != nil || b.cancel != nil {
		b.mu.Unlock()

		return ErrAlreadyEnabled
	}
	b.mu.Unlock()

	b.setStatus(tunStatusStarting, "")

	// 1. Capability gate (defense in depth — the selector already
	//    checked; the transaction refuses to proceed otherwise).
	if !supportsFirstPartyTUN(opts.Config) {
		b.failEnable(errors.New("tunnel: first-party TUN: configuration beyond the FreeIran Engine capability set"))

		return fmt.Errorf("tunnel: enable tun: %w", ErrUnsupportedPlatform)
	}

	// 2. Elevation — required to create the adapter and configure the
	//    interface. Checked BEFORE any mutation (no UAC prompt here;
	//    the UI elevates).
	if !tunProcessElevated() {
		b.failEnable(errors.New("tunnel: first-party TUN: requires elevation"))

		return ErrRequiresElevation
	}

	// 3. Collision-free FreeIran identity + addressing: the
	//    deterministic identity is re-derived per activation; the
	//    address plan refuses collisions against CURRENT observation
	//    for BOTH families (an IPv6 plan colliding with a live host
	//    address is just as fatal as an IPv4 one).
	identity := fctun.DefaultIdentity()
	ipv4 := netip.MustParsePrefix(freecore.DefaultTUNPrefix4)
	ipv6 := netip.MustParsePrefix(freecore.DefaultTUNPrefix6)

	observation, err := tunObserve()
	if err != nil {
		b.failEnable(fmt.Errorf("tunnel: first-party TUN: observe: %w", err))

		return err
	}

	for _, fact := range observation.Interfaces {
		for _, prefix := range fact.Addresses {
			addr := prefix.Addr()

			if !addr.IsValid() || factOwnedName(fact.Name, identity) {
				continue
			}

			if (addr.Is4() && ipv4.Contains(addr)) || (addr.Is6() && ipv6.Contains(addr)) {
				b.failEnable(fmt.Errorf("tunnel: first-party TUN: address plan collides with %s", fact.Name))

				return fmt.Errorf("tunnel: first-party TUN: address collision")
			}
		}
	}

	// 4. Open the Wintun device (FreeIran-owned adapter identity).
	device, err := fctun.OpenDevice(ctx, identity, fctun.DeviceOptions{})
	if err != nil {
		b.failEnable(fmt.Errorf("tunnel: first-party TUN: open device: %w", err))

		return err
	}

	b.mu.Lock()
	b.device = device
	b.identity = identity
	b.mu.Unlock()

	rollback := make([]func() error, 0, 8)

	rollbackFailed := func(cause error) error {
		b.undoAll(rollback)
		_ = device.Close()
		b.mu.Lock()
		b.device = nil
		b.mu.Unlock()
		b.failEnable(cause)

		return cause
	}

	// 5. Configure the interface state (addresses + covering routes for
	//    BOTH families) through the FreeIran-owned additive IP surface,
	//    recording undos. The mutator only ever ADDS exact entries —
	//    a family-wide flush (the pre-0.14.1 primitive) would destroy
	//    unrelated user-created addresses.
	luidProvider, ok := device.(interface{ LUID() uint64 })
	if !ok {
		return rollbackFailed(errors.New("tunnel: first-party TUN: device does not expose its LUID"))
	}

	undos, err := fctun.ApplyInterfaceConfig(fctun.InterfaceConfig{
		LUID:        luidProvider.LUID(),
		Addresses:   []netip.Prefix{ipv4, ipv6},
		Routes:      fctun.DefaultCoveredRoutes(),
		RouteMetric: 0,
	})
	if err != nil {
		return rollbackFailed(fmt.Errorf("tunnel: first-party TUN: interface config: %w", err))
	}

	rollback = append(rollback, undos...)

	// 6. Start the engine (flows only) and the userspace stack. The
	//    upstream dialer carries the loop-prevention constraint: the
	//    physical interface is observed NOW, live — never hardcoded,
	//    never the TUN. A missing physical interface fails the
	//    activation CLOSED (an unbound upstream is a loop risk).
	constraint := buildUpstreamConstraint(observation, identity, ipv4)
	if constraint == nil {
		return rollbackFailed(errors.New("tunnel: first-party TUN: no physical upstream interface available (fail-closed)"))
	}

	if err := constraint.Validate(); err != nil {
		return rollbackFailed(fmt.Errorf("tunnel: first-party TUN: upstream constraint: %w", err))
	}

	route, rerr := firstPartyRouteOf(opts.Config)
	if rerr != nil {
		return rollbackFailed(fmt.Errorf("tunnel: first-party TUN: normalize route: %w", rerr))
	}

	engine, err := freecore.NewEngine(freecore.Options{
		Route:          route,
		UpstreamDialer: freecore.NewUpstreamDialer(constraint),
	})
	if err != nil {
		return rollbackFailed(fmt.Errorf("tunnel: first-party TUN: engine: %w", err))
	}

	engineCtx, cancel := context.WithCancel(ctx)

	if err := engine.ServeFlowsOnly(engineCtx); err != nil {
		cancel()

		return rollbackFailed(fmt.Errorf("tunnel: first-party TUN: engine start: %w", err))
	}

	dataplane, err := freecore.NewTUNDataplane(engine, freecore.TUNDataplaneOptions{
		Device:  device,
		Addr4:   ipv4.Addr(),
		Prefix4: ipv4,
		Addr6:   ipv6.Addr(),
		Prefix6: ipv6,
		// UpstreamDialer: the engine's outbounds dial through the
		// constrained dialer so upstreams cannot loop into the TUN.
	})
	if err != nil {
		cancel()

		return rollbackFailed(fmt.Errorf("tunnel: first-party TUN: userspace stack: %w", err))
	}

	go func() { _ = dataplane.Run(engineCtx) }()

	go func() { _ = engine.Wait(engineCtx) }()

	b.mu.Lock()
	b.cancel = cancel
	b.dataplane = dataplane
	b.rollback = rollback
	b.ipv4 = ipv4
	b.ipv6 = ipv6
	b.startedAt = time.Now().UTC()
	b.configured = opts.Config.DisplayURL()
	b.mu.Unlock()

	// 7. Verification gate — the EVIDENCE LADDER (v0.14.1). The rungs
	//    actually executed here: platform-ready (elevation), stack-ready
	//    (adapter + engine + userspace stack), route-ready (identity,
	//    covered routes owned by the TUN interface in BOTH families,
	//    physical upstream present). Traffic-verified is NOT claimed:
	//    nothing here proves application bytes crossed the TUN on a
	//    physical host, and unit/in-memory evidence must never be
	//    converted into runtime evidence. See TUNSnapshot's evidence
	//    fields and docs/tun.md.
	evidence, verified := b.verify(observation, identity, constraint)
	if !verified {
		_ = b.Disable(ctx)

		err := errors.New("tunnel: first-party TUN: verification gate failed")
		b.failEnable(err)

		return err
	}

	b.mu.Lock()
	b.evidence = evidence
	b.mu.Unlock()

	b.setStatus(tunStatusActive, "")

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "tun_active",
		Message:   fmt.Sprintf("first-party TUN active: adapter %s, %s + %s, engine-owned dataplane (evidence: route-ready; traffic-verified not claimed)", identity.AdapterName, ipv4, ipv6),
		Status:    "active",
	})

	return nil
}

// tunEvidence is the honest evidence ladder of one first-party
// activation. Each rung is EARNED by an actual gate; Active is the
// route-ready rung — it must never be read as Level-5 application
// traffic proof.
type tunEvidence struct {
	// Compiled: the first-party dataplane is built into this binary.
	Compiled bool

	// PlatformReady: elevation + platform gates passed on this host.
	PlatformReady bool

	// StackReady: adapter open, engine serving, userspace stack running.
	StackReady bool

	// RouteReady: adapter identity observed, covering routes owned by
	// the FreeIran interface in BOTH families, physical upstream
	// present and validated.
	RouteReady bool

	// TrafficVerified: application traffic observed crossing the
	// first-party TUN. NEVER set by the activation gate — there is no
	// probe here that produces that evidence, and fabricating it (or
	// promoting unit/in-memory evidence) is forbidden. It becomes true
	// only when a real runtime traffic probe exists.
	TrafficVerified bool
}

// verify runs the observable gates of the activation and returns the
// earned evidence ladder.
func (b *freecoreTUNBackend) verify(prior fctun.Observation, identity fctun.Identity, constraint *fctun.UpstreamConstraint) (tunEvidence, bool) {
	evidence := tunEvidence{Compiled: true}

	// (a) Platform: the transaction got this far only elevated — that
	//     rung is earned by construction.
	evidence.PlatformReady = true

	// (b) The adapter is observed with the expected identity and is up.
	fresh, err := tunObserve()
	if err != nil {
		return evidence, false
	}

	iface, found := fresh.FindInterface(identity.AdapterName)
	if !found || !iface.Running {
		return evidence, false
	}

	evidence.StackReady = true

	// (c) Route ownership, BOTH families: the covered IPv4 routes are
	//     held by FreeIran's interface index (IPv4 table observation),
	//     and every covered route (v4 AND v6) resolves through THIS
	//     interface's LUID via the dual-stack row2 lookup — the exact
	//     TUN interface owns them, not merely "the routes exist".
	guard := fctun.LoopGuard{
		AdapterName:    identity.AdapterName,
		InterfaceIndex: iface.Index,
		TunAddresses:   []netip.Prefix{b.ipv4, b.ipv6},
		CoveredRoutes:  fctun.DefaultCoveredRoutes(),
	}

	if !fresh.CoveringRoutesHeld(guard) {
		return evidence, false
	}

	if luidProvider, ok := b.device.(interface {
		LUID() uint64
	}); ok {
		if err := fctun.VerifyRoutesOwnedByLUID(luidProvider.LUID(), fctun.DefaultCoveredRoutes()); err != nil {
			return evidence, false
		}
	} else {
		return evidence, false
	}

	// (d) The physical upstream interface still exists, is distinct
	//     from the TUN, and the constraint validates (the exact
	//     fail-closed shapes — missing physical, physical == TUN — are
	//     refused here too, not only at dial time).
	if constraint == nil || constraint.Validate() != nil {
		return evidence, false
	}

	physical, upstreamFound := fresh.FindInterfaceByIndex(constraint.PhysicalInterfaceIndex)
	if !upstreamFound || !physical.Running {
		return evidence, false
	}

	evidence.RouteReady = true

	// TrafficVerified stays FALSE by design — see the type doc.
	return evidence, true
}

// buildUpstreamConstraint derives the loop-prevention policy from live
// OS observation. The TUN interface index and the PHYSICAL interface
// index are two DIFFERENT facts recorded in two DIFFERENT fields: the
// guard carries the TUN's own index; the physical default-route owner
// (excluding the TUN) is observed from the forwarding table. A missing
// physical interface yields nil and the activation fails closed — the
// dialer will never silently fall back to unbound (loop-prone) or
// TUN-bound (looping) sockets.
func buildUpstreamConstraint(observation fctun.Observation, identity fctun.Identity, tunPrefix netip.Prefix) *fctun.UpstreamConstraint {
	tunIndex := uint32(0)

	if iface, found := observation.FindInterface(identity.AdapterName); found {
		tunIndex = iface.Index
	}

	hints := observation.DefaultInterfaceHints(tunIndex)
	if len(hints) == 0 {
		return nil
	}

	guard := fctun.LoopGuard{
		AdapterName:    identity.AdapterName,
		InterfaceIndex: tunIndex,
		TunAddresses:   []netip.Prefix{tunPrefix},
	}

	constraint := guard.Constraint(hints[0].Index)

	return &constraint
}

func factOwnedName(name string, identity fctun.Identity) bool {
	return identity.OwnsName(name)
}

// Disable tears the session down transactionally (mirror of Enable).
func (b *freecoreTUNBackend) Disable(_ context.Context) error {
	b.mu.Lock()
	device := b.device
	cancel := b.cancel
	b.mu.Unlock()

	if device == nil && cancel == nil {
		b.setStatus(tunStatusOff, "")

		return nil
	}

	b.setStatus(tunStatusStopping, "")

	if cancel != nil {
		cancel()
	}

	// Undo exactly the FreeIran-owned mutations, verify residuals, and
	// surface incomplete cleanup honestly.
	residual := b.undoAll(nil)

	if device != nil {
		_ = device.Close()
	}

	b.mu.Lock()
	b.device = nil
	b.cancel = nil
	b.dataplane = nil
	b.rollback = nil
	b.mu.Unlock()

	if residual > 0 {
		err := fmt.Errorf("tunnel: first-party TUN: %d rollback step(s) incomplete (residual state surfaced honestly)", residual)
		b.setStatus(tunStatusFailed, err.Error())

		return err
	}

	b.setStatus(tunStatusOff, "")

	return nil
}

// Snapshot is the redacted truth about the first-party dataplane.
// The evidence ladder is part of the truth: Active means the
// route-ready rung — TrafficVerified stays false until a real runtime
// traffic probe exists (never fabricated).
func (b *freecoreTUNBackend) Snapshot() TUNSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	snap := TUNSnapshot{
		Available:         true,
		Installed:         true,
		InterfaceName:     b.identity.AdapterName,
		IPv4Address:       prefixString(b.ipv4),
		IPv6Address:       prefixString(b.ipv6),
		Routes:            coveredRouteStrings(),
		RequiresElevation: true,
		Backend:           tunDataplaneFirstParty,
		Active:            b.status == tunStatusActive,
		Status:            b.status,
		Configuration:     b.configured,
		Details:           b.details,
	}

	// The evidence rungs actually earned by this backend's gates.
	snap.Compiled = true
	snap.PlatformReady = b.evidence.PlatformReady
	snap.StackReady = b.evidence.StackReady
	snap.RouteReady = b.evidence.RouteReady
	snap.TrafficVerified = b.evidence.TrafficVerified

	return snap
}

// undoAll runs the recorded undo closures in REVERSE order (the
// transactional contract). Returns the count of steps that failed —
// residual state the caller must surface, never swallow.
func (b *freecoreTUNBackend) undoAll(rollback []func() error) int {
	b.mu.Lock()
	steps := b.rollback
	b.rollback = nil
	b.mu.Unlock()

	if rollback != nil {
		steps = rollback
	}

	failed := 0

	for i := len(steps) - 1; i >= 0; i-- {
		if err := steps[i](); err != nil {
			failed++

			logging.LogR(logging.Record{
				Level:     logging.LevelWarn,
				Subsystem: Subsystem,
				Event:     "tun_rollback_step_failed",
				Message:   fmt.Sprintf("first-party TUN rollback step %d: %v", i, err),
				Status:    "rollback",
			})
		}
	}

	return failed
}

// failEnable records an honest pre-Active failure.
func (b *freecoreTUNBackend) failEnable(err error) {
	b.setStatus(tunStatusFailed, err.Error())

	logging.LogR(logging.Record{
		Level:     logging.LevelWarn,
		Subsystem: Subsystem,
		Event:     "tun_failed",
		Message:   err.Error(),
		Status:    "failed",
	})
}

// prefixString renders a prefix redacted-safe (addresses only, no
// credentials ever exist here).
func prefixString(p netip.Prefix) string {
	if !p.IsValid() {
		return ""
	}

	return p.String()
}

func coveredRouteStrings() []string {
	out := make([]string, 0, 3)

	for _, r := range fctun.DefaultCoveredRoutes() {
		out = append(out, r.String())
	}

	return out
}

// firstPartyRouteOf extracts the normalized route for the engine from
// the enable options (the same Normalize the registry path uses).
func firstPartyRouteOf(cfg config.Config) (freecore.Route, error) {
	return freecore.Normalize(cfg)
}

// strings guard for platform builds that trim diagnostics.
var _ = strings.TrimSpace
