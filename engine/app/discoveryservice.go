// discoveryservice.go implements the v0.9.6 adaptive start flow and
// the discovery service surface (§5/§15):
//
//	START → DETECT → DISCOVER → TEST → RANK → CONNECT → VERIFY → MONITOR
//
// The user should not need to understand every protocol: pressing
// Start runs the flow. Advanced controls (manual candidate selection,
// test-mode configuration) remain available and MANUAL SELECTION
// ALWAYS OVERRIDES automatic selection.
//
// The service composes the v0.9.6 engine layers:
//
//   - engine/netcheck environment intelligence decides the discovery
//     LEVELS (a restricted environment escalates to deep discovery);
//   - engine/discovery produces the validated, deduplicated pool;
//   - the selected test mode (engine/tester ModeTester) measures the
//     pool's top candidates;
//   - engine/ranking ranks by the user's sort mode with separated
//     metric scores;
//   - engine/connection connects (optionally racing the top 2-4) and
//     VERIFIES usable connectivity;
//   - the existing recovery supervisor keeps MONITORing afterwards.
//
// Every stage emits real progress events with its measured duration;
// nothing is animated or fabricated.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/discovery"
	"github.com/Parsaetak/FreeIran/engine/netcheck"
	"github.com/Parsaetak/FreeIran/engine/pipeline"
	"github.com/Parsaetak/FreeIran/engine/ranking"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/tester"
)

// StartFlowStage names one stage of the adaptive start flow.
type StartFlowStage string

const (
	FlowStageDetecting   StartFlowStage = "detecting"
	FlowStageDiscovering StartFlowStage = "discovering"
	FlowStageTesting     StartFlowStage = "testing"
	FlowStageRanking     StartFlowStage = "ranking"
	FlowStageConnecting  StartFlowStage = "connecting"
	FlowStageVerifying   StartFlowStage = "verifying"
	FlowStageConnected   StartFlowStage = "connected"
	FlowStageFailed      StartFlowStage = "failed"
	FlowStageNoUsable    StartFlowStage = "no_usable_candidates"
	FlowStageIdle        StartFlowStage = "idle"
)

// StartFlowEvent is one progress event of the start flow.
type StartFlowEvent struct {
	Stage      StartFlowStage `json:"stage"`
	Message    string         `json:"message,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
	At         int64          `json:"at"`
}

// StartFlowStatus is the current status of the flow.
type StartFlowStatus struct {
	Stage       StartFlowStage               `json:"stage"`
	Running     bool                         `json:"running"`
	Message     string                       `json:"message,omitempty"`
	StartedAt   int64                        `json:"started_at,omitempty"`
	FinishedAt  int64                        `json:"finished_at,omitempty"`
	LastRunMS   int64                        `json:"last_run_ms,omitempty"`
	Environment []netcheck.EnvironmentSignal `json:"environment,omitempty"`
	LastResult  *StartFlowResult             `json:"last_result,omitempty"`
}

// StartFlowResult summarizes one completed flow run.
type StartFlowResult struct {
	Discovered           int    `json:"discovered"`
	Valid                int    `json:"valid"`
	Duplicates           int    `json:"duplicates"`
	Tested               int    `json:"tested"`
	ConnectedFingerprint string `json:"connected_fingerprint,omitempty"`
	Verified             bool   `json:"verified"`
	FailureClass         string `json:"failure_class,omitempty"`
	DurationMS           int64  `json:"duration_ms"`
}

// DiscoveryService exposes the discovery engine, environment
// intelligence and the adaptive start flow to the UI.
type DiscoveryService struct {
	app *App

	mu       sync.Mutex
	engine   *discovery.Engine
	analyzer *netcheck.EnvironmentAnalyzer
	envCache netcheck.Environment
	envAt    time.Time

	flowMu       sync.Mutex
	flowRunning  bool
	flowStatus   StartFlowStatus
	flowCancel   context.CancelFunc
	flowListener func(StartFlowEvent)
}

// NewDiscoveryService binds the discovery service to the app.
func NewDiscoveryService(a *App) *DiscoveryService {
	s := &DiscoveryService{app: a}
	s.flowStatus.Stage = FlowStageIdle

	return s
}

// ensureEngine lazily constructs the discovery engine bound to the
// app's workspace (health persists beside the other state files).
func (s *DiscoveryService) ensureEngine() *discovery.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.engine != nil {
		return s.engine
	}

	s.engine = discovery.NewEngine(nil, discovery.DefaultEngineConfig(),
		s.app.layout.Config+"/discovery-health.json")

	return s.engine
}

// SetFlowListener installs the event listener used by the runtime to
// forward start-flow progress to the UI.
func (s *DiscoveryService) SetFlowListener(fn func(StartFlowEvent)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.flowListener = fn
}

func (s *DiscoveryService) emit(ev StartFlowEvent) {
	ev.At = time.Now().UTC().UnixMilli()

	s.mu.Lock()
	fn := s.flowListener
	s.mu.Unlock()

	if fn != nil {
		fn(ev)
	}
}

// Environment returns the cached environment analysis, refreshing it
// when older than the cache window (5 minutes).
func (s *DiscoveryService) Environment(ctx context.Context) netcheck.Environment {
	s.mu.Lock()
	age := time.Since(s.envAt)
	cached := s.envCache
	s.mu.Unlock()

	if age < 5*time.Minute && !cached.AnalyzedAt.IsZero() {
		return cached
	}

	analyzer := s.ensureAnalyzer()

	env := analyzer.Analyze(ctx)

	s.mu.Lock()
	s.envCache = env
	s.envAt = time.Now()
	s.mu.Unlock()

	return env
}

func (s *DiscoveryService) ensureAnalyzer() *netcheck.EnvironmentAnalyzer {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.analyzer != nil {
		return s.analyzer
	}

	s.analyzer = netcheck.NewEnvironmentAnalyzer(netcheck.DefaultEnvironmentConfig())

	return s.analyzer
}

// levelsForEnvironment maps environment evidence onto discovery
// levels: a restrictive environment escalates; a captive portal
// reports the sign-in problem instead (discovery cannot fix an
// unauthenticated link).
func (s *DiscoveryService) levelsForEnvironment(env netcheck.Environment) discovery.LevelSet {
	if env.Has(netcheck.SignalCaptivePortal) {
		return discovery.StandardLevels()
	}

	if env.DeepDiscoveryAdvised || env.Restricted {
		return discovery.DeepLevels()
	}

	return discovery.FullLevels()
}

// DiscoverNow runs one discovery pass at the requested depth and
// persists the newly discovered candidates into the store. This is
// the manual "Discover now" control.
func (s *DiscoveryService) DiscoverNow(deep bool) (*discovery.Stats, error) {
	if s.app.ctx == nil {
		return nil, fmt.Errorf("app: not started")
	}

	engine := s.ensureEngine()

	levels := discovery.FullLevels()
	if deep {
		levels = discovery.DeepLevels()
	}

	s.app.mu.RLock()
	sources := append([]source.Source(nil), s.app.sources...)
	s.app.mu.RUnlock()

	cached := s.app.knownConfigs()

	nodes, stats := engine.Discover(s.app.ctx, levels, cached, sources)

	s.persistDiscovered(nodes)

	return stats, nil
}

// knownConfigs loads the stored configuration pool (bounded by the
// same candidateScanLimit the ranking snapshot uses).
func (a *App) knownConfigs() []config.Config {
	out := make([]config.Config, 0, 256)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = a.store.Iterate(ctx, func(key string, raw []byte) error {
		if len(out) >= candidateScanLimit {
			return errCandidateLimit
		}

		var cfg config.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil // undecodable record: skip, never abort
		}

		if cfg.ID == "" {
			cfg.ID = key
		}

		out = append(out, cfg)

		return nil
	})

	return out
}

// persistDiscovered writes discovery-level candidates into the store
// through the same sink the classic pipeline uses (new fingerprints
// only; duplicates were already removed upstream).
func (s *DiscoveryService) persistDiscovered(nodes []discovery.Node) {
	if s.app == nil || s.app.store == nil || len(nodes) == 0 {
		return
	}

	fresh := make([]config.Config, 0, len(nodes))

	for _, node := range nodes {
		if node.Level == discovery.LevelCached {
			continue // already stored
		}

		fresh = append(fresh, node.Config)
	}

	if len(fresh) == 0 {
		return
	}

	sink := pipeline.NewStoreSink(s.app.store, 512)

	if err := sink.Persist(context.Background(), fresh); err != nil {
		s.app.logger.Warn("discovery", "persist_failed",
			"persisting %d discovered candidates failed: %v", len(fresh), err)

		return
	}

	_ = sink.Flush()

	s.app.InvalidateRankingSnapshot()

	s.app.logger.Info("discovery", "nodes_persisted",
		"persisted %d newly discovered candidates", len(fresh))
}

// RunStartFlow executes the full adaptive flow:
// DETECT → DISCOVER → TEST → RANK → CONNECT → VERIFY. Manual
// selection override: a non-empty manualFingerprint connects exactly
// that candidate, skipping automatic selection.
func (s *DiscoveryService) RunStartFlow(manualFingerprint string) (*StartFlowResult, error) {
	s.flowMu.Lock()

	if s.flowRunning {
		s.flowMu.Unlock()

		return nil, fmt.Errorf("start flow already running")
	}

	s.flowRunning = true
	s.flowStatus = StartFlowStatus{
		Stage:     FlowStageDetecting,
		Running:   true,
		StartedAt: time.Now().UTC().UnixMilli(),
	}

	ctx, cancel := context.WithCancel(s.app.ctx)
	s.flowCancel = cancel

	s.flowMu.Unlock()

	defer func() {
		s.flowMu.Lock()
		s.flowRunning = false
		s.flowCancel = nil
		s.flowStatus.Running = false
		s.flowStatus.FinishedAt = time.Now().UTC().UnixMilli()
		s.flowMu.Unlock()

		cancel()
	}()

	started := time.Now()

	result := &StartFlowResult{}

	emitStage := func(stage StartFlowStage, msg string, took time.Duration, detail map[string]any) {
		s.flowMu.Lock()
		s.flowStatus.Stage = stage
		s.flowStatus.Message = msg
		s.flowMu.Unlock()

		s.emit(StartFlowEvent{
			Stage: stage, Message: msg,
			DurationMS: took.Milliseconds(), Detail: detail,
		})
	}

	// ---- 1. DETECT ------------------------------------------------
	detectStart := time.Now()

	env := s.Environment(ctx)

	s.flowMu.Lock()
	s.flowStatus.Environment = env.Signals
	s.flowMu.Unlock()

	emitStage(FlowStageDetecting, env.Summary, time.Since(detectStart), map[string]any{
		"signals": env.Signals, "restricted": env.Restricted,
	})

	// ---- 2. DISCOVER ----------------------------------------------
	discStart := time.Now()

	engine := s.ensureEngine()

	levels := s.levelsForEnvironment(env)

	s.app.mu.RLock()
	sources := append([]source.Source(nil), s.app.sources...)
	s.app.mu.RUnlock()

	cached := s.app.knownConfigs()

	nodes, stats := engine.Discover(ctx, levels, cached, sources)

	s.persistDiscovered(nodes)

	result.Discovered = stats.Discovered
	result.Valid = stats.Valid
	result.Duplicates = stats.Duplicates

	emitStage(FlowStageDiscovering, fmt.Sprintf(
		"%d valid candidates (%d duplicates removed)", result.Valid, result.Duplicates),
		time.Since(discStart), map[string]any{
			"levels": levelNames(levels),
		})

	if len(nodes) == 0 {
		result.DurationMS = time.Since(started).Milliseconds()

		s.finishFlow(FlowStageNoUsable, "no usable candidates discovered", result)

		return result, nil
	}

	// ---- 3. TEST (mode-driven, top candidates) --------------------
	testStart := time.Now()

	tested := s.testTopCandidates(ctx, nodes)

	result.Tested = tested

	emitStage(FlowStageTesting, fmt.Sprintf("%d candidates measured", tested),
		time.Since(testStart), nil)

	// ---- 4. RANK --------------------------------------------------
	rankStart := time.Now()

	ranked := s.rankCandidates(nodes)

	emitStage(FlowStageRanking, fmt.Sprintf("%d candidates ranked", len(ranked)),
		time.Since(rankStart), nil)

	if len(ranked) == 0 {
		result.DurationMS = time.Since(started).Milliseconds()

		s.finishFlow(FlowStageNoUsable, "no usable candidates after ranking", result)

		return result, nil
	}

	// ---- 5. CONNECT (manual override) -----------------------------
	connStart := time.Now()

	// Route-trust policy (v0.9.8.6, extended to the discovery flow in
	// v0.10.5): the AUTOMATIC selection path considers TRUSTED routes
	// only — official and user-configured sources — unless the user
	// explicitly allowed untrusted public routes. This is the same
	// boundary Quick Connect enforces (quickconnect.go): a reachable
	// public node is not automatically trusted, and discovery-driven
	// auto-connect is exactly the path the policy governs. MANUAL
	// selection (manualFingerprint) is explicit user choice and is
	// never filtered — public nodes stay fully reachable through
	// explicit selection.
	if manualFingerprint == "" && !s.app.currentSettings().AllowUntrustedPublicRoutes {
		trusted := make([]config.Config, 0, len(ranked))
		excludedPublic := 0

		for _, cfg := range ranked {
			if cfg.RouteTrusted() {
				trusted = append(trusted, cfg)
			} else {
				excludedPublic++
			}
		}

		if len(trusted) == 0 && excludedPublic > 0 {
			s.app.logger.Warn("discovery", "startflow_untrusted_only",
				"%d candidates from untrusted public sources were excluded by the route-trust policy",
				excludedPublic)

			result.DurationMS = time.Since(started).Milliseconds()

			s.finishFlow(FlowStageNoUsable,
				"all ranked candidates came from untrusted public sources and automatic selection is restricted to trusted routes",
				result)

			return result, nil
		}

		ranked = trusted
	}

	var connected *config.Config

	if manualFingerprint != "" {
		// MANUAL SELECTION OVERRIDES automatic selection.
		for i := range ranked {
			if ranked[i].Fingerprint() == manualFingerprint {
				cfg := ranked[i]
				connected = &cfg

				break
			}
		}

		if connected == nil {
			if raw, err := s.app.store.Get(manualFingerprint); err == nil {
				var cfg config.Config
				if json.Unmarshal(raw, &cfg) == nil {
					connected = &cfg
				}
			}
		}

		if connected == nil {
			result.DurationMS = time.Since(started).Milliseconds()

			s.finishFlow(FlowStageFailed, "manual candidate not found", result)

			return result, fmt.Errorf("start flow: manual candidate %s not found", manualFingerprint)
		}
	}

	if connected == nil {
		connected = s.connectBest(ctx, ranked)
	}

	if connected == nil {
		result.DurationMS = time.Since(started).Milliseconds()

		s.finishFlow(FlowStageFailed, "connection attempts failed", result)

		return result, nil
	}

	result.ConnectedFingerprint = connected.Fingerprint()

	emitStage(FlowStageConnecting, "connected via "+string(connected.Type), time.Since(connStart), nil)

	// ---- 6. VERIFY ------------------------------------------------
	verifyStart := time.Now()

	verify, err := s.app.connMgr.VerifyConnected(ctx, connection.VerifyOptions{})

	result.Verified = verify.OK

	if !verify.OK {
		result.FailureClass = string(verify.FailureClass)
	}

	emitStage(FlowStageVerifying, verify.Describe(), time.Since(verifyStart), nil)

	// ---- Done -----------------------------------------------------
	result.DurationMS = time.Since(started).Milliseconds()

	if result.Verified {
		s.finishFlow(FlowStageConnected, "connected and verified", result)
	} else {
		s.finishFlow(FlowStageFailed,
			"connected but verification failed: "+verify.Describe(), result)
	}

	_ = err

	return result, nil
}

func (s *DiscoveryService) finishFlow(stage StartFlowStage, msg string, result *StartFlowResult) {
	s.flowMu.Lock()
	s.flowStatus.Stage = stage
	s.flowStatus.Message = msg
	s.flowStatus.LastResult = result
	s.flowMu.Unlock()

	s.emit(StartFlowEvent{Stage: stage, Message: msg})
}

// testTopCandidates runs the configured test mode against candidates
// without fresh measurements (bounded by the mode's candidate cap).
func (s *DiscoveryService) testTopCandidates(ctx context.Context, nodes []discovery.Node) int {
	mode := s.app.currentTestMode()

	modeTester := tester.NewModeTester(s.app.coreRegistry, mode.Options)

	pending := make([]discovery.Node, 0, len(nodes))

	for _, n := range nodes {
		if n.Ping == nil && n.URLTest == nil {
			pending = append(pending, n)
		}
	}

	if len(pending) > mode.MaxCandidates {
		pending = pending[:mode.MaxCandidates]
	}

	tested := 0

	for i := range pending {
		if ctx.Err() != nil {
			break
		}

		cfg := pending[i].Config

		// v0.9.15 one-engine guard: a fingerprint already queued or
		// in flight is skipped — the shared queue stays the single
		// test executor and single last writer for that config.
		if s.app.queueHas(cfg.Fingerprint()) {
			continue
		}

		out := modeTester.TestAndApplyMode(ctx, &cfg)

		if out.Ping != nil || out.URLTest != nil {
			if raw, err := json.Marshal(&cfg); err == nil {
				if err := s.app.store.Upsert(cfg.Fingerprint(), raw); err == nil {
					tested++
				}
			}
		}
	}

	if tested > 0 {
		s.app.InvalidateRankingSnapshot()
	}

	return tested
}

// rankCandidates builds the rich candidate list and ranks it by the
// user's selected sort mode, returning only connectable candidates.
func (s *DiscoveryService) rankCandidates(nodes []discovery.Node) []config.Config {
	settings := s.app.currentSettings()

	sortMode := ranking.SortMode(settings.SortMode)

	rich := make([]ranking.RichCandidate, 0, len(nodes))

	byFingerprint := make(map[string]config.Config, len(nodes))

	for _, n := range nodes {
		fp := n.Fingerprint()
		if fp == "" {
			continue
		}

		byFingerprint[fp] = n.Config

		rich = append(rich, ranking.RichCandidate{
			Candidate: ranking.Candidate{
				Fingerprint:        fp,
				Name:               n.Name,
				Protocol:           string(n.Type),
				Endpoint:           n.Address,
				Source:             n.SourceID,
				CompatibleBackends: s.app.backendCountFor(n.Type),
			},
			Ping:          n.Ping,
			URLTest:       n.URLTest,
			Handshake:     n.Handshake,
			LastSuccessAt: n.LastSuccessAt,
			FailureStreak: n.FailureStreak,

			// v0.11.0 failure evidence (from the embedded config
			// record) feeds the same protocol-specific demotion as
			// the classic ranking surface.
			LastFailureAt:    n.LastFailureAt,
			LastFailureClass: n.LastFailureClass,
		})
	}

	ranked := ranking.RankMetrics(rich, sortMode, time.Now().UTC())

	out := make([]config.Config, 0, len(ranked))

	for _, r := range ranked {
		if r.Score.Connectable {
			if cfg, ok := byFingerprint[r.Candidate.Fingerprint]; ok {
				out = append(out, cfg)
			}
		}
	}

	return out
}

// connectBest connects the best candidate, racing the top 2-4 when
// the user enabled racing and enough candidates exist.
func (s *DiscoveryService) connectBest(ctx context.Context, ranked []config.Config) *config.Config {
	if len(ranked) == 0 {
		return nil
	}

	settings := s.app.currentSettings()

	if settings.EnableRacing && len(ranked) >= 2 {
		racers := settings.RacingCandidates
		if racers < 2 {
			racers = 2
		}

		if racers > 4 {
			racers = 4
		}

		if racers > len(ranked) {
			racers = len(ranked)
		}

		outcome, err := connection.Race(ctx, connection.RaceOptions{
			Candidates: ranked[:racers],
			Racers:     racers,
			Registry:   s.app.coreRegistry,
			Verify:     connection.VerifyOptions{},
		})

		if err == nil && outcome.Winner != nil {
			if _, cerr := s.app.connMgr.Connect(ctx, *outcome.Winner, corePreferences(settings)); cerr == nil {
				return outcome.Winner
			}
		}
	}

	// Sequential: best first, bounded attempts.
	for i := range ranked {
		if ctx.Err() != nil {
			return nil
		}

		cfg := ranked[i]

		if _, err := s.app.connMgr.Connect(ctx, cfg, corePreferences(settings)); err == nil {
			return &cfg
		}
	}

	return nil
}

// CancelStartFlow cancels a running flow (user action).
func (s *DiscoveryService) CancelStartFlow() bool {
	s.flowMu.Lock()
	cancel := s.flowCancel
	s.flowMu.Unlock()

	if cancel != nil {
		cancel()

		return true
	}

	return false
}

// StartFlowStatus returns the current flow status.
func (s *DiscoveryService) StartFlowStatus() StartFlowStatus {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()

	return s.flowStatus
}

// SourceHealthList returns the measured health of every known source
// (the Source Intelligence surface, §16).
func (s *DiscoveryService) SourceHealthList() []discovery.SourceHealth {
	engine := s.ensureEngine()

	return engine.Health().Snapshot()
}

// backendCountFor counts the installed cores that can serve a
// protocol type.
func (a *App) backendCountFor(t config.Type) int {
	if a.coreRegistry == nil {
		return 0
	}

	count := 0

	for _, backend := range a.coreRegistry.Backends() {
		if backend.Status != core.StatusAvailable {
			continue
		}

		if c, ok := a.coreRegistry.Get(backend.Name); ok {
			if c.Supports(config.Config{Type: t}) {
				count++
			}
		}
	}

	return count
}

// currentTestMode resolves the user's test-mode selection (§9) with
// safe defaults.
func (a *App) currentTestMode() TestModeSelection {
	settings := a.currentSettings()

	sel := TestModeSelection{
		Options: tester.ModeOptions{
			Mode:        tester.Mode(settings.TestMode),
			PingSamples: settings.TestPingSamples,
			URL:         settings.TestURL,
			URLTimeout:  time.Duration(settings.TestURLTimeoutSeconds) * time.Second,
		},
		MaxCandidates: settings.TestMaxCandidates,
	}

	if sel.Options.Mode == "" {
		sel.Options.Mode = tester.ModePingURL
	}

	if sel.MaxCandidates <= 0 {
		sel.MaxCandidates = 20
	}

	if sel.MaxCandidates > 200 {
		sel.MaxCandidates = 200
	}

	return sel
}

// corePreferences derives the backend-selection preferences from the
// user's settings (preferred backend honoured when compatible).
func corePreferences(settings Settings) core.Preferences {
	pref := core.Preferences{AllowFallback: true}

	if settings.PreferredBackend != "" {
		pref.PreferredBackend = settings.PreferredBackend
	}

	return pref
}

// levelNames renders the executed level names for progress events.
func levelNames(l discovery.LevelSet) []string {
	var out []string

	for _, lv := range []struct {
		active bool
		name   string
	}{
		{l.Cached, discovery.LevelName(discovery.LevelCached)},
		{l.Configured, discovery.LevelName(discovery.LevelConfigured)},
		{l.TrustedPublic, discovery.LevelName(discovery.LevelTrustedPublic)},
		{l.Search, discovery.LevelName(discovery.LevelSearch)},
		{l.Content, discovery.LevelName(discovery.LevelContent)},
		{l.Recovery, discovery.LevelName(discovery.LevelRecovery)},
		{l.Deep, discovery.LevelName(discovery.LevelDeep)},
	} {
		if lv.active {
			out = append(out, lv.name)
		}
	}

	sort.Strings(out)

	return out
}

// TestModeSelection is the resolved test-mode configuration (§9).
type TestModeSelection struct {
	// Options drive the ModeTester.
	Options tester.ModeOptions

	// MaxCandidates caps how many candidates one flow tests.
	MaxCandidates int
}

// PublicSourceDiscovery runs one bounded public-source discovery
// cycle (v0.9.7 §7): the GitHub adapter plus the generic HTTP
// connector feed the shared candidate queue; validated material lands
// in the staging ledger with full provenance. Configured sources are
// never displaced and discovered nodes are NOT auto-tested.
func (s *DiscoveryService) PublicSourceDiscovery() (*discovery.GitHubOutcome, error) {
	if s.app.publicSources == nil {
		return nil, fmt.Errorf("public discovery unavailable")
	}

	return s.app.publicSources.Run(s.app.ctx)
}

// PublicSourceStaged returns the staged discovered sources (§9
// provenance ledger view for the UI).
func (s *DiscoveryService) PublicSourceStaged() []stagingEntry {
	return s.app.LoadStaged()
}

// PublicSourceRateLimits returns the per-provider request accounting
// (v0.9.7 §8): requests, successes, failures, 429/403 counters,
// remaining budget and cooldown state.
func (s *DiscoveryService) PublicSourceRateLimits() []discovery.ProviderStats {
	if s.app.publicSources == nil {
		return nil
	}

	return s.app.publicSources.RateLimitStats()
}
