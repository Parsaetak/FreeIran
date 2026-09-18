// providerservice.go exposes the unified provider architecture to
// the UI (§12/§13) and implements evidence-based Auto provider
// selection. Tor and Psiphon are first-class providers — never node
// protocols; their sessions run through the EXISTING connection
// engine's lifecycle (select → start → ready → route → verify →
// connected → monitor → recover).
package app

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/provider"
	"github.com/Parsaetak/FreeIran/engine/ranking"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// ProviderMode values (§12).
const (
	ProviderModeAuto    = "auto"
	ProviderModeConfigs = "configs"
	ProviderModeTor     = "tor"
	ProviderModePsiphon = "psiphon"
)

// AllProviderModes is the UI choice list.
var AllProviderModes = []string{
	ProviderModeAuto,
	ProviderModeConfigs,
	ProviderModeTor,
	ProviderModePsiphon,
}

// ProviderModeLabel renders a UI label.
func ProviderModeLabel(mode string) string {
	switch mode {
	case ProviderModeAuto:
		return "Auto"
	case ProviderModeConfigs:
		return "Configurations"
	case ProviderModeTor:
		return "Tor"
	case ProviderModePsiphon:
		return "Psiphon"
	default:
		return "Auto"
	}
}

// providerRecord is the evidence trail for one provider (§12):
// availability, health, verification, latency, stability, recent
// success — all from REAL session outcomes, never hardcoded
// priority.
type providerRecord struct {
	LastSuccessAt   time.Time
	LastFailureAt   time.Time
	FailureStreak   int
	LastLatencyMS   int64
	LatencyMeasured bool
	Starts          int
}

// ProviderEvidence tracks provider session outcomes in memory.
type ProviderEvidence struct {
	mu      sync.Mutex
	records map[string]*providerRecord
}

// newProviderEvidence creates the tracker.
func newProviderEvidence() *ProviderEvidence {
	return &ProviderEvidence{records: map[string]*providerRecord{}}
}

func (e *ProviderEvidence) record(name string) *providerRecord {
	if e.records[name] == nil {
		e.records[name] = new(providerRecord)
	}

	return e.records[name]
}

// RecordSuccess records a verified provider session.
func (e *ProviderEvidence) RecordSuccess(name string, latencyMS int64, measured bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rec := e.record(name)
	rec.LastSuccessAt = time.Now().UTC()
	rec.FailureStreak = 0
	rec.LastLatencyMS = latencyMS
	rec.LatencyMeasured = measured
}

// RecordFailure records a failed provider session.
func (e *ProviderEvidence) RecordFailure(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rec := e.record(name)
	rec.LastFailureAt = time.Now().UTC()
	rec.FailureStreak++
}

// snapshot copies the evidence for scoring.
func (e *ProviderEvidence) snapshot(name string) providerRecord {
	e.mu.Lock()
	defer e.mu.Unlock()

	if rec := e.records[name]; rec != nil {
		return *rec
	}

	return providerRecord{}
}

// ProviderChoice is one scored option for Auto selection.
type ProviderChoice struct {
	Kind    string   `json:"kind"` // "configs" | "tor" | "psiphon"
	Name    string   `json:"name"` // display name
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons,omitempty"` // explainable, like ranking
}

// AutoSelect ranks the provider options by evidence (§12): a
// verified-recent, healthy, fast, stable option wins; availability
// is a hard gate. Nothing is chosen from hardcoded priority.
func (a *App) AutoSelect(ctx context.Context) ([]ProviderChoice, string, error) {
	choices := []ProviderChoice{}

	// ---- Configurations: the existing ranking evidence engine ----
	configScore, configReasons := a.autoConfigEvidence(ctx)

	choices = append(choices, ProviderChoice{
		Kind:    ProviderModeConfigs,
		Name:    "Configurations",
		Score:   configScore,
		Reasons: configReasons,
	})

	// ---- Tor / Psiphon: provider evidence ------------------------
	for _, name := range []string{provider.TorName, provider.PsiphonName} {
		score, reasons, available := a.autoProviderEvidence(ctx, name)
		if !available {
			continue
		}

		choices = append(choices, ProviderChoice{
			Kind:    name,
			Name:    name,
			Score:   score,
			Reasons: reasons,
		})
	}

	if len(choices) == 0 {
		return nil, "", fmt.Errorf("no provider option is available")
	}

	// Deterministic order: score desc, then kind.
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].Score != choices[j].Score {
			return choices[i].Score > choices[j].Score
		}

		return choices[i].Kind < choices[j].Kind
	})

	return choices, choices[0].Kind, nil
}

// autoConfigEvidence derives the configurations option's score from
// the real ranking snapshot (when one exists).
func (a *App) autoConfigEvidence(ctx context.Context) (float64, []string) {
	reasons := []string{}

	// A store with candidates: use the best candidate's evidence.
	candidates := a.collectCandidates(ctx)
	if len(candidates) == 0 {
		return 20, []string{"no tested configurations yet"}
	}

	_, score, ok := ranking.SelectBest(candidates, time.Now().UTC())
	if !ok {
		return 25, []string{"no viable configuration candidate"}
	}

	if score.LatencyMSMeasured {
		if score.LatencyMS <= 0 {
			reasons = append(reasons, "best candidate < 1 ms median")
		} else {
			reasons = append(reasons, fmt.Sprintf("best candidate %d ms median", score.LatencyMS))
		}
	}

	reasons = append(reasons, fmt.Sprintf("%d candidates ranked", len(candidates)))

	// The classic composite score (0-100) maps onto the provider
	// scale directly — same evidence vocabulary.
	return score.Score, reasons
}

// autoProviderEvidence scores one provider from real outcomes.
func (a *App) autoProviderEvidence(ctx context.Context, name string) (float64, []string, bool) {
	prov, ok := a.providerMgr.Get(name)
	if !ok {
		return 0, nil, false
	}

	info := prov.Info()

	if !info.Installed {
		// Uninstalled providers are unavailable options (never
		// auto-installed: §7/§8 require explicit user action).
		return 0, nil, false
	}

	reasons := []string{"installed"}
	score := 30.0 // installed baseline

	// Health (live measurement when running; last known otherwise).
	health := prov.Health(ctx)
	if health.OK {
		score += 25
		reasons = append(reasons, "healthy")

		if health.Measured {
			if health.LatencyMS <= 0 {
				reasons = append(reasons, "endpoint < 1 ms")
			} else {
				reasons = append(reasons, fmt.Sprintf("endpoint %d ms", health.LatencyMS))
			}
		}
	} else if prov.State() == provider.StateReady {
		score += 10
		reasons = append(reasons, "ready")
	}

	// Verification / recent success evidence.
	rec := a.providerEvidence.snapshot(name)

	if !rec.LastSuccessAt.IsZero() {
		age := time.Since(rec.LastSuccessAt)
		switch {
		case age <= 30*time.Minute:
			score += 25
			reasons = append(reasons, "verified recently")
		case age <= 6*time.Hour:
			score += 15
			reasons = append(reasons, "verified today")
		default:
			score += 5
			reasons = append(reasons, "verified "+humanDuration(age)+" ago")
		}
	}

	// Stability: failure streak penalty.
	if rec.FailureStreak > 0 {
		penalty := float64(rec.FailureStreak) * 8
		if penalty > 30 {
			penalty = 30
		}

		score -= penalty
		reasons = append(reasons, fmt.Sprintf("%d consecutive failures", rec.FailureStreak))
	}

	// Latency from the last verified session.
	if rec.LatencyMeasured {
		switch {
		case rec.LastLatencyMS <= 150:
			score += 10
		case rec.LastLatencyMS <= 400:
			score += 6
		case rec.LastLatencyMS <= 800:
			score += 3
		}
	}

	if score < 0 {
		score = 0
	}

	return score, reasons, true
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// ProviderService is the Wails-bound provider surface (§13).
type ProviderService struct {
	app *App
}

// NewProviderService creates the service.
func NewProviderService(a *App) *ProviderService {
	return &ProviderService{app: a}
}

// Modes lists the provider choices with labels (§12).
func (s *ProviderService) Modes() []ProviderModeView {
	views := make([]ProviderModeView, 0, len(AllProviderModes))

	for _, mode := range AllProviderModes {
		views = append(views, ProviderModeView{
			Mode:  mode,
			Label: ProviderModeLabel(mode),
		})
	}

	return views
}

// ProviderModeView is one UI choice.
type ProviderModeView struct {
	Mode  string `json:"mode"`
	Label string `json:"label"`
}

// Mode returns the effective provider mode from settings.
func (s *ProviderService) Mode() string {
	mode := s.app.currentSettings().ProviderMode
	switch mode {
	case ProviderModeConfigs, ProviderModeTor, ProviderModePsiphon, ProviderModeAuto:
		return mode
	default:
		return ProviderModeAuto
	}
}

// SetMode persists the provider mode choice.
func (s *ProviderService) SetMode(mode string) (string, error) {
	switch mode {
	case ProviderModeConfigs, ProviderModeTor, ProviderModePsiphon, ProviderModeAuto, "":
	default:
		return "", fmt.Errorf("unknown provider mode %q", mode)
	}

	if err := s.app.persistSettings(func(settings *Settings) {
		settings.ProviderMode = mode
	}); err != nil {
		return s.Mode(), err
	}

	return s.Mode(), nil
}

// List renders every provider's Info (§13: installed, version,
// healthy, runtime state, source, license/notice, last check).
func (s *ProviderService) List() []provider.Info {
	if s.app.providerMgr == nil {
		return nil
	}

	return s.app.providerMgr.List()
}

// Info renders one provider.
func (s *ProviderService) Info(name string) (provider.Info, error) {
	prov, ok := s.app.providerMgr.Get(name)
	if !ok {
		return provider.Info{}, fmt.Errorf("unknown provider %q", name)
	}

	return prov.Info(), nil
}

// Health measures one provider (bounded).
func (s *ProviderService) Health(name string) (provider.Health, error) {
	prov, ok := s.app.providerMgr.Get(name)
	if !ok {
		return provider.Health{}, fmt.Errorf("unknown provider %q", name)
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 15*time.Second)
	defer cancel()

	return prov.Health(ctx), nil
}

// Install runs a provider's managed download pipeline (long-running;
// progress through structured logs).
func (s *ProviderService) Install(name string) (provider.Info, error) {
	prov, ok := s.app.providerMgr.Get(name)
	if !ok {
		return provider.Info{}, fmt.Errorf("unknown provider %q", name)
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 10*time.Minute)
	defer cancel()

	if err := prov.Install(ctx); err != nil {
		return prov.Info(), err
	}

	return prov.Info(), nil
}

// Uninstall removes a provider's binaries and runtime data.
func (s *ProviderService) Uninstall(name string) (provider.Info, error) {
	prov, ok := s.app.providerMgr.Get(name)
	if !ok {
		return provider.Info{}, fmt.Errorf("unknown provider %q", name)
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 30*time.Second)
	defer cancel()

	if err := prov.Uninstall(ctx); err != nil {
		return prov.Info(), err
	}

	return prov.Info(), nil
}

// Start launches a provider's managed instance WITHOUT establishing
// a connection session (advanced; Quick Connect uses Connect).
func (s *ProviderService) Start(name string) (provider.Info, error) {
	prov, ok := s.app.providerMgr.Get(name)
	if !ok {
		return provider.Info{}, fmt.Errorf("unknown provider %q", name)
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 3*time.Minute)
	defer cancel()

	if err := prov.Start(ctx); err != nil {
		return prov.Info(), err
	}

	return prov.Info(), nil
}

// Stop shuts a provider's managed instance down.
func (s *ProviderService) Stop(name string) (provider.Info, error) {
	prov, ok := s.app.providerMgr.Get(name)
	if !ok {
		return provider.Info{}, fmt.Errorf("unknown provider %q", name)
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 30*time.Second)
	defer cancel()

	if err := prov.Stop(ctx); err != nil {
		return prov.Info(), err
	}

	return prov.Info(), nil
}

// TorOptionsView carries the user's Tor bridge configuration.
type TorOptionsView struct {
	BridgeLines      []string          `json:"bridge_lines,omitempty"`
	TransportPlugins map[string]string `json:"transport_plugins,omitempty"`
}

// SetTorOptions validates and applies bridge configuration (§8: no
// hardcoded bridges; user-provided only, validated before launch).
func (s *ProviderService) SetTorOptions(view TorOptionsView) error {
	options := provider.TorOptions{
		BridgeLines:      view.BridgeLines,
		TransportPlugins: view.TransportPlugins,
	}

	if err := provider.ValidateTorOptions(options); err != nil {
		return err
	}

	if err := s.app.persistSettings(func(settings *Settings) {
		settings.TorBridgeLines = view.BridgeLines
		settings.TorTransportPlugins = view.TransportPlugins
	}); err != nil {
		return err
	}

	return s.app.torEngine.SetOptions(options)
}

// SetPsiphonExtraConfig validates and applies advanced Psiphon
// config.
func (s *ProviderService) SetPsiphonExtraConfig(extra string) error {
	options := provider.PsiphonOptions{ExtraConfig: extra}

	if err := s.app.psiphonEngine.SetOptions(options); err != nil {
		return err
	}

	return s.app.persistSettings(func(settings *Settings) {
		settings.PsiphonExtraConfig = extra
	})
}

// SetPsiphonUserBinary adopts a user-provided binary (validated).
func (s *ProviderService) SetPsiphonUserBinary(path string) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 60*time.Second)
	defer cancel()

	if err := s.app.psiphonEngine.SetUserBinary(ctx, path); err != nil {
		return err
	}

	return s.app.persistSettings(func(settings *Settings) {
		settings.PsiphonUserBinary = path
	})
}

// AutoChoices returns the evidence-scored provider options and the
// winner (§12). Explainable, like configuration ranking.
func (s *ProviderService) AutoChoices() ([]ProviderChoice, string, error) {
	ctx, cancel := context.WithTimeout(s.app.ctx, 45*time.Second)
	defer cancel()

	return s.app.AutoSelect(ctx)
}

// Connect establishes a session through one provider — through the
// EXISTING connection lifecycle (start → ready → verify → connected).
func (s *ProviderService) Connect(name string) (connection.Snapshot, error) {
	prov, ok := s.app.providerMgr.Get(name)
	if !ok {
		return connection.Snapshot{}, fmt.Errorf("unknown provider %q", name)
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 4*time.Minute)
	defer cancel()

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: provider.Subsystem,
		Event:     "provider_session_start",
		Message:   name + ": provider session",
		Fields:    map[string]any{"provider": name},
	})

	snapshot, err := s.app.connMgr.ConnectProvider(ctx, prov, connection.VerifyOptions{})
	if err != nil {
		s.app.providerEvidence.RecordFailure(name)

		logging.LogR(logging.Record{
			Level:     logging.LevelWarn,
			Subsystem: provider.Subsystem,
			Event:     "provider_session_failed",
			Message:   name + ": " + err.Error(),
			Fields:    map[string]any{"provider": name, "error_kind": "connect"},
		})

		return snapshot, err
	}

	s.app.providerEvidence.RecordSuccess(name, snapshot.LatencyMS, snapshot.Verification == "usable")

	logging.LogR(logging.Record{
		Level:      logging.LevelInfo,
		Subsystem:  provider.Subsystem,
		Event:      "provider_session_connected",
		Message:    name + ": verified usable",
		DurationMS: 0,
		Fields: map[string]any{
			"provider":   name,
			"latency_ms": snapshot.LatencyMS,
			"verified":   snapshot.Verification,
		},
	})

	return snapshot, nil
}

// ConnectAuto implements the Auto provider mode (§12): evidence-based
// selection, then the same connection lifecycle. Falls back to the
// classic configuration flow when configurations win the evidence.
func (s *ProviderService) ConnectAuto() (connection.Snapshot, string, error) {
	ctx, cancel := context.WithTimeout(s.app.ctx, 45*time.Second)
	defer cancel()

	choices, winner, err := s.app.AutoSelect(ctx)
	if err != nil {
		return connection.Snapshot{}, "", err
	}

	_ = choices

	switch winner {
	case ProviderModeTor, ProviderModePsiphon:
		snapshot, cerr := s.Connect(winner)

		return snapshot, winner, cerr
	default:
		// Configurations win: the EXISTING ConnectBest flow.
		result, cerr := NewConnectionService(s.app).ConnectBest(nil)
		if cerr != nil {
			return connection.Snapshot{}, ProviderModeConfigs, cerr
		}

		return result.Snapshot, ProviderModeConfigs, nil
	}
}

// CurrentProvider reports the active session's provider ("" for
// configuration sessions).
func (s *ProviderService) CurrentProvider() string {
	return s.app.connMgr.ProviderName()
}
