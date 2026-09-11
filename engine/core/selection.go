package core

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/config"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// Preferences guide deterministic backend selection.
type Preferences struct {
	// PreferredBackend is a user-selected backend name ("" = none).
	// A preference only wins when the backend is compatible AND
	// available; it never overrides capability incompatibility.
	PreferredBackend string

	// AllowFallback permits trying compatible backends after the
	// preferred/primary candidate fails during connection setup.
	AllowFallback bool

	// MaxAttempts bounds the number of backends tried for one
	// connection (default DefaultMaxAttempts).
	MaxAttempts int
}

// WithDefaults applies selection defaults.
func (p Preferences) WithDefaults() Preferences {
	resolved := p

	if resolved.MaxAttempts <= 0 {
		resolved.MaxAttempts = DefaultMaxAttempts
	}

	return resolved
}

// DefaultMaxAttempts bounds fallback so a failure can never cycle
// endlessly through backends.
const DefaultMaxAttempts = 3

// Selection is the deterministic outcome of backend resolution: the
// chosen core, the human-readable reason, and the ordered fallback
// candidates. Every selection is explainable after the fact.
type Selection struct {
	Core      Core
	Reason    string
	Fallbacks []string
}

// Select resolves which backend executes a configuration.
//
// The algorithm is deterministic and explainable:
//
//  1. every registered backend is scored for capability compatibility
//     (Supports) and runtime availability;
//  2. candidates are ordered: user preference first (when compatible
//     and available), then registry priority, then name;
//  3. the winner is returned with its reason; the remaining
//     compatible available backends are the ordered fallbacks.
//
// Selection never uses randomness or heuristics beyond declared
// capabilities: identical inputs produce identical outcomes.
func (r *Registry) Select(cfg config.Config, pref Preferences) (Selection, error) {
	if r == nil {
		return Selection{}, firerrors.New(firerrors.KindFatal,
			Subsystem, "select", "registry is nil")
	}

	cfg.Normalize()

	pref = pref.WithDefaults()

	r.mu.RLock()

	type candidate struct {
		info BackendInfo
		core Core
	}

	candidates := make([]candidate, 0, len(r.backends))

	for name, entry := range r.backends {
		var caps Capabilities

		if provider, ok := entry.core.(interface {
			Capabilities() Capabilities
		}); ok {
			caps = provider.Capabilities()
		}

		candidates = append(candidates, candidate{
			info: BackendInfo{
				Name:         name,
				Status:       entry.status,
				Version:      entry.version,
				Path:         entry.path,
				Priority:     entry.priority,
				Capabilities: caps,
				Summary:      caps.Summary(),
				LastCheck:    entry.check,
				Note:         entry.note,
			},
			core: entry.core,
		})
	}

	r.mu.RUnlock()

	// Score: compatible + available candidates only.
	eligible := make([]candidate, 0, len(candidates))
	incompatible := make([]string, 0, len(candidates))
	unavailable := make([]string, 0, len(candidates))

	for _, cand := range candidates {
		if !cand.core.Supports(cfg) {
			incompatible = append(incompatible, cand.info.Name)
			continue
		}

		if cand.info.Status != StatusAvailable {
			unavailable = append(unavailable, cand.info.Name)
			continue
		}

		eligible = append(eligible, cand)
	}

	if len(eligible) == 0 {
		details := describeNoCandidate(cfg, incompatible, unavailable)

		return Selection{}, firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "select",
			"no compatible backend for %s (%s)",
			cfg.DisplayURL(), details)
	}

	// Deterministic ordering: preference → priority → name.
	sort.Slice(eligible, func(i, j int) bool {
		return lessCandidate(eligible[i].info, eligible[j].info, pref.PreferredBackend)
	})

	chosen := eligible[0]

	fallbacks := make([]string, 0, len(eligible)-1)

	for _, cand := range eligible[1:] {
		fallbacks = append(fallbacks, cand.info.Name)
	}

	reason := buildReason(chosen.info, cfg, pref, len(eligible), len(eligible)-1)

	return Selection{
		Core:      chosen.core,
		Reason:    reason,
		Fallbacks: fallbacks,
	}, nil
}

// lessCandidate orders two eligible candidates deterministically.
func lessCandidate(a, b BackendInfo, preferred string) bool {
	aPref := a.Name == preferred && preferred != ""
	bPref := b.Name == preferred && preferred != ""

	if aPref != bPref {
		return aPref
	}

	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}

	return a.Name < b.Name
}

// buildReason renders the explainable selection reason. It never
// includes credential material.
func buildReason(info BackendInfo, cfg config.Config, pref Preferences, eligible, fallbacks int) string {
	var builder strings.Builder

	builder.WriteString(info.Name)
	builder.WriteString(": ")

	switch {
	case info.Name == pref.PreferredBackend && pref.PreferredBackend != "":
		builder.WriteString("preferred backend, ")
	case info.Priority == 0:
		builder.WriteString("highest-priority backend, ")
	default:
		builder.WriteString("priority ")
	}

	builder.WriteString("supports ")
	builder.WriteString(string(cfg.Type))

	if cfg.Network != "" && cfg.Network != string(config.NetworkTCP) {
		builder.WriteString("/")
		builder.WriteString(cfg.Network)
	}

	if cfg.Security != "" && cfg.Security != string(config.SecurityNone) {
		builder.WriteString(" with ")
		builder.WriteString(cfg.Security)
	}

	builder.WriteString("; ")
	builder.WriteString(strconv.Itoa(eligible))
	builder.WriteString(" candidate(s), ")
	builder.WriteString(strconv.Itoa(fallbacks))
	builder.WriteString(" fallback(s), version ")
	builder.WriteString(info.Version)

	return builder.String()
}

// describeNoCandidate renders why selection failed: which backends
// were incompatible, which were unavailable.
func describeNoCandidate(cfg config.Config, incompatible, unavailable []string) string {
	parts := make([]string, 0, 2)

	if len(incompatible) > 0 {
		parts = append(parts, "incompatible: "+strings.Join(incompatible, ", "))
	}

	if len(unavailable) > 0 {
		parts = append(parts, "installed but unavailable: "+strings.Join(unavailable, ", "))
	}

	if len(parts) == 0 {
		parts = append(parts, "no backends registered")
	}

	return strings.Join(parts, "; ")
}
