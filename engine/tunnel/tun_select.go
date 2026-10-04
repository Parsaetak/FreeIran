package tunnel

import (
	"context"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/freecore"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// tunDataplaneFirstParty is the Backend label of the first-party TUN
// dataplane (diagnostics + UI truth about WHO owns the wire).
const tunDataplaneFirstParty = "FreeIran Engine TUN (first-party userspace stack)"

// firstPartyTUN is the capability-gated first-party TUN backend. On
// platforms where the first-party dataplane exists (Windows) it is the
// PREFERRED TUN authority for configurations the FreeIran Engine
// genuinely supports; everything else keeps using the managed sing-box
// dataplane. Selection happens per Enable (per configuration), is
// logged explicitly, and the two dataplanes are mutually exclusive for
// one activation — never both launched, never one left active after
// the other claims ownership.
type selectingTUNBackend struct {
	firstParty TUNBackend // nil when the platform has no first-party dataplane
	fallback   TUNBackend // sing-box managed dataplane (or unavailable)

	active TUNBackend // the backend that owns the current session (nil = off)
}

func newSelectingTUNBackend(firstParty, fallback TUNBackend) TUNBackend {
	return &selectingTUNBackend{firstParty: firstParty, fallback: fallback}
}

// Available reports whether ANY TUN dataplane can be served.
func (s *selectingTUNBackend) Available() bool {
	if s.firstParty != nil && s.firstParty.Available() {
		return true
	}

	return s.fallback.Available()
}

// Install keeps the sing-box dependency set healthy (the first-party
// dataplane needs no external install — Wintun ships in its embedded
// driver form through the platform layer).
func (s *selectingTUNBackend) Install(ctx context.Context) error {
	return s.fallback.Install(ctx)
}

// Enable runs the activation transaction on the selected dataplane.
func (s *selectingTUNBackend) Enable(ctx context.Context, opts TUNEnableOptions) error {
	if s.firstParty != nil && s.firstParty.Available() && supportsFirstPartyTUN(opts.Config) {
		logging.LogR(logging.Record{
			Level:     logging.LevelInfo,
			Subsystem: Subsystem,
			Event:     "tun_backend_selected",
			Message:   "first-party TUN dataplane selected (FreeIran Engine supports this configuration)",
			Status:    "starting",
		})

		if err := s.firstParty.Enable(ctx, opts); err != nil {
			return err
		}

		s.active = s.firstParty

		return nil
	}

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "tun_backend_selected",
		Message:   "sing-box TUN dataplane selected (configuration beyond the first-party capability set or platform gate false)",
		Status:    "starting",
	})

	if err := s.fallback.Enable(ctx, opts); err != nil {
		return err
	}

	s.active = s.fallback

	return nil
}

// Disable tears down whichever dataplane owns the session.
func (s *selectingTUNBackend) Disable(ctx context.Context) error {
	backend := s.active
	if backend == nil {
		// Nothing active: mirror the fallback's Disable for state
		// hygiene (it is idempotent and restores stale markers).
		return s.fallback.Disable(ctx)
	}

	err := backend.Disable(ctx)
	s.active = nil

	return err
}

// Snapshot reports the active dataplane (or the honest aggregate view
// when nothing is active).
func (s *selectingTUNBackend) Snapshot() TUNSnapshot {
	if s.active != nil {
		return s.active.Snapshot()
	}

	if s.firstParty != nil && s.firstParty.Available() {
		snap := s.firstParty.Snapshot()
		if snap.Backend == "" {
			snap.Backend = tunDataplaneFirstParty
		}

		return snap
	}

	return s.fallback.Snapshot()
}

// supportsFirstPartyTUN is the capability gate: the FreeIran Engine
// must genuinely support the configuration (deterministic capability
// match — no preference, no fallback involvement).
func supportsFirstPartyTUN(cfg config.Config) bool {
	if cfg.Type == "" {
		return false
	}

	return firstPartyTUNEngine().Supports(cfg)
}

// firstPartyTUNEngine returns the first-party capability oracle. It is
// the SAME backend the registry uses — one capability truth for
// System Proxy and TUN alike.
func firstPartyTUNEngine() interface {
	Supports(config.Config) bool
} {
	return freecore.New()
}
