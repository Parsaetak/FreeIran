package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/coremgr"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/tester"
	"github.com/Parsaetak/FreeIran/engine/testqueue"
	"github.com/Parsaetak/FreeIran/engine/tunnel"
)

// CoreLifecycleView is the complete UI-facing lifecycle projection of
// one managed core (manifest + registry discovery + failure text).
//
// v0.9.14: Origin and Ownership surface WHERE the active binary came
// from and whether FreeIran owns it or only references an external
// installation; StatusNote carries honest non-failure remarks ("newer
// than stable"); LastDecision is the reuse decision of the last
// install call.
type CoreLifecycleView struct {
	Manifest       coremgr.Manifest `json:"manifest"`
	Discovered     bool             `json:"discovered"`
	RuntimeState   string           `json:"runtime_state"`
	RuntimeVersion string           `json:"runtime_version,omitempty"`
	Path           string           `json:"path,omitempty"`
	FailureMessage string           `json:"failure_message,omitempty"`
	Origin         string           `json:"origin,omitempty"`
	Ownership      string           `json:"ownership,omitempty"`
	StatusNote     string           `json:"status_note,omitempty"`
	LastDecision   string           `json:"last_decision,omitempty"`
}

// CoreService exposes the Managed Core Manager to the UI. Every method
// is a thin wrapper around the corresponding coremgr.Manager method
// so the Wails binding layer can expose them as a namespaced API.
type CoreService struct {
	app *App
}

// NewCoreService binds a core service to the app.
func NewCoreService(a *App) *CoreService {
	return &CoreService{app: a}
}

// ensureCoreMgr returns the app's core manager. v0.9.0: the manager
// is created eagerly in app.New because its bin directories are core
// discovery inputs, so this only surfaces the shared instance (the
// old lazy path could create a second manager whose install locations
// the running registry never learned about).
func (s *CoreService) ensureCoreMgr() (*coremgr.Manager, error) {
	if s.app.coreMgr == nil {
		return nil, fmt.Errorf("app: core manager is not initialized")
	}

	return s.app.coreMgr, nil
}

// refreshAfterLifecycle re-runs core discovery so Connect/Backends/
// tester observe install/remove/update changes immediately.
func (s *CoreService) refreshAfterLifecycle() {
	s.app.RefreshCores()
}

// List returns every managed core's manifest.
func (s *CoreService) List() []coremgr.Manifest {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return nil
	}
	return mgr.All()
}

// Info returns the manifest for one core.
func (s *CoreService) Info(name coremgr.CoreName) (coremgr.Manifest, error) {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return coremgr.Manifest{}, err
	}
	mf, _ := mgr.Info(name)
	return mf, nil
}

// Install downloads and activates the latest stable release for one
// core. This is a long-running operation; progress is delivered via
// the freeiran:coreprogress event.
func (s *CoreService) Install(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}

	err = mgr.Install(s.app.ctx, name)
	s.refreshAfterLifecycle()

	return err
}

// Uninstall removes one core and its manifest.
func (s *CoreService) Uninstall(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}

	err = mgr.Remove(s.app.ctx, name)
	s.refreshAfterLifecycle()

	return err
}

// HealthCheck re-runs the smoke test for one core.
func (s *CoreService) HealthCheck(name coremgr.CoreName) (coremgr.HealthResult, error) {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return coremgr.HealthResult{}, err
	}
	return mgr.HealthCheck(s.app.ctx, name)
}

// HealthCheckAll runs smoke tests for every core in parallel.
func (s *CoreService) HealthCheckAll() map[coremgr.CoreName]coremgr.HealthResult {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return nil
	}
	return mgr.HealthCheckAll(s.app.ctx)
}

// CheckForUpdates queries the upstream release API for one core.
func (s *CoreService) CheckForUpdates(name coremgr.CoreName) (coremgr.UpdateInfo, error) {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return coremgr.UpdateInfo{}, err
	}
	return mgr.CheckForUpdates(s.app.ctx, name)
}

// CheckAllForUpdates queries every core in parallel.
func (s *CoreService) CheckAllForUpdates() []coremgr.UpdateInfo {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return nil
	}
	return mgr.CheckAllForUpdates(s.app.ctx)
}

// Rollback reverts one core to its previously retained version.
func (s *CoreService) Rollback(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}

	err = mgr.Rollback(s.app.ctx, name)
	s.refreshAfterLifecycle()

	return err
}

// Repair tries to fix a broken core: rollback first, fresh install
// second. Discovery is refreshed afterwards so a repaired binary is
// immediately usable.
func (s *CoreService) Repair(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}

	err = mgr.Repair(s.app.ctx, name)
	s.refreshAfterLifecycle()

	return err
}

// Reinstall removes every local artifact of a core and performs a
// fresh install from upstream.
func (s *CoreService) Reinstall(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}

	err = mgr.Reinstall(s.app.ctx, name)
	s.refreshAfterLifecycle()

	return err
}

// UpdateAll checks every core for updates and installs what is
// newer. Returns a per-core outcome map.
func (s *CoreService) UpdateAll() map[coremgr.CoreName]error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return nil
	}

	out := mgr.UpdateAll(s.app.ctx)
	s.refreshAfterLifecycle()

	return out
}

// SetChannel changes the release channel.
func (s *CoreService) SetChannel(name coremgr.CoreName, channel coremgr.Channel) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}
	return mgr.SetChannel(name, channel)
}

// Disable marks a core as disabled.
func (s *CoreService) Disable(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}
	return mgr.Disable(name)
}

// Enable re-activates a disabled core.
func (s *CoreService) Enable(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}

	err = mgr.Enable(s.app.ctx, name)
	s.refreshAfterLifecycle()

	return err
}

// LifecycleInfo is the UI projection of one core: the persisted
// manifest plus the live runtime view (registry availability) and a
// human-readable failure explanation. The runtime state covers the
// Starting/Running/Stopping/Failed half of the lifecycle that does
// not persist across restarts.
func (s *CoreService) LifecycleInfo() []CoreLifecycleView {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return nil
	}

	registry := map[string]core.BackendInfo{}
	for _, info := range s.app.coreRegistry.Backends() {
		registry[info.Name] = info
	}

	views := make([]CoreLifecycleView, 0, len(coremgr.AllCores))

	for _, mf := range mgr.All() {
		view := CoreLifecycleView{
			Manifest:       mf,
			RuntimeState:   "not_running",
			FailureMessage: mgr.ExplainFailure(mf.Name),
			Origin:         mf.Origin,
			Ownership:      mf.OwnershipLabel(),
			StatusNote:     mf.StatusNote,
			LastDecision:   mf.LastDecision,
		}

		if info, ok := registry[string(mf.Name)]; ok && info.Status == core.StatusAvailable {
			view.Discovered = true
			view.RuntimeVersion = info.Version
			view.Path = info.Path

			// The registry's discovery result is authoritative for
			// provenance when the manifest predates the field or the
			// active binary was adopted externally.
			if view.Origin == "" {
				view.Origin = info.Origin
			}

			if view.Ownership == "" {
				view.Ownership = info.Ownership
			}
		}

		views = append(views, view)
	}

	// v0.9.14 state reconciliation: a core the registry discovered
	// but coremgr has no manifest for (a PATH/system installation
	// never managed by FreeIran) must STILL appear in the Cores UI —
	// the user must never have to install a second copy just to make
	// the app aware of an existing installation. The synthesized view
	// carries an honest external/ready state; install remains
	// available but unnecessary.
	seen := make(map[coremgr.CoreName]bool, len(views))
	for _, view := range views {
		seen[view.Manifest.Name] = true
	}

	for _, info := range s.app.coreRegistry.Backends() {
		if info.Status != core.StatusAvailable {
			continue
		}

		name := coremgr.CoreName(info.Name)

		if seen[name] {
			continue
		}

		views = append(views, CoreLifecycleView{
			Manifest: coremgr.Manifest{
				Name:        name,
				State:       coremgr.StateReady,
				Version:     info.Version,
				BinaryPath:  info.Path,
				Ownership:   info.Ownership,
				Origin:      info.Origin,
				LastChecked: info.LastCheck,
			},
			Discovered:     true,
			RuntimeState:   "not_running",
			RuntimeVersion: info.Version,
			Path:           info.Path,
			Origin:         info.Origin,
			Ownership:      info.Ownership,
			StatusNote:     "already installed on this system",
		})
	}

	return views
}

// --- Test Queue Service ---

// TestQueueService exposes the test queue to the UI.
type TestQueueService struct {
	app *App
}

// NewTestQueueService binds a test queue service to the app.
func NewTestQueueService(a *App) *TestQueueService {
	return &TestQueueService{app: a}
}

// ensureTestQueue returns the app's ONE test queue, initializing it
// lazily. initMu serializes the init so concurrent callers don't
// create duplicate queues (which would leak the prior queue's
// workers). v0.9.15: this is the SINGLE construction path — the
// TestQueueService AND DataService.TestConfig both answer from it, so
// every testing entry point shares one execution engine.
func (a *App) ensureTestQueue() (*testqueue.Queue, error) {
	a.initMu.Lock()
	defer a.initMu.Unlock()

	if a.testQueue != nil {
		return a.testQueue, nil
	}

	cfg := testqueue.DefaultConfig()

	// Start the queue at the memory booster's CURRENT adapted
	// settings (worker count + queue depth), so a late-created queue
	// respects the pressure regime the controller already computed.
	// currentSettings is nil-receiver-safe and returns the static
	// defaults when the controller is absent.
	if settings := a.memory.currentSettings(); settings.QueueConcurrency > 0 {
		cfg.Concurrency = settings.QueueConcurrency
	}

	if settings := a.memory.currentSettings(); settings.QueueDepth > 0 {
		cfg.MaxQueueSize = settings.QueueDepth
	}

	adapter := &testerAdapter{app: a}
	q := testqueue.New(adapter, cfg)
	q.Start(a.ctx)

	a.testQueue = q
	return q, nil
}

// ensureQueue returns the app's test queue (delegates to the shared
// App-level construction path).
func (s *TestQueueService) ensureQueue() (*testqueue.Queue, error) {
	return s.app.ensureTestQueue()
}

// testerAdapter bridges the existing tester.Tester (which takes a
// config.Config) to the testqueue.Tester interface (which takes a
// fingerprint + backends list). The adapter looks up the
// configuration by fingerprint in the store, then runs the existing
// tester against it.
type testerAdapter struct {
	app *App
}

// Test implements testqueue.Tester. It:
//  1. looks up the configuration by fingerprint in the store;
//  2. runs the tester (the app's chained probe: real core + e2e ping,
//     TCP reachability fallback);
//  3. maps the tester.Result to a testqueue.Result;
//  4. PERSISTS the outcome to the store so the latest test data is
//     displayed without retesting (v0.9.0 §4 — v0.8 dropped queue
//     results on the floor).
func (a *testerAdapter) Test(ctx context.Context, fingerprint string, backends []string) (testqueue.Result, error) {
	_ = backends // backend selection is capability-driven inside the probe chain

	// 1. Look up the config.
	raw, err := a.app.store.Get(fingerprint)
	if err != nil {
		return testqueue.Result{
			TestedAt:  time.Now().UTC(),
			LastError: fmt.Sprintf("config not found: %v", err),
		}, err
	}

	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return testqueue.Result{
			TestedAt:  time.Now().UTC(),
			LastError: fmt.Sprintf("decode config: %v", err),
		}, err
	}

	// 2. Run the tester.
	result := a.app.tester.Test(ctx, cfg)

	// 3. Map to the queue result shape.
	qr := testqueue.Result{
		Working:   result.Working,
		Latency:   result.Latency,
		Measured:  result.Measured,
		Backend:   result.Backend,
		TestedAt:  result.TestedAt,
		LastError: result.LastError,

		Protocol:   result.Protocol,
		Endpoint:   result.Endpoint,
		PingMS:     result.PingMS,
		DurationMS: result.DurationMS,
		Quality:    result.Quality,
	}

	if !result.Working {
		qr.FailureCategory = testqueue.ClassifyByError(result.LastError)
	}

	// 4. Persist the updated runtime fields.
	tester.ApplyResult(&cfg, result)

	if updated, mErr := json.Marshal(cfg); mErr == nil {
		if uErr := a.app.store.Upsert(fingerprint, updated); uErr != nil {
			a.app.logger.Warn("testqueue", "persist_result_failed",
				"could not persist test result for %s: %v", fingerprint, uErr)
		}
	}

	// A persisted test result changed the ranking inputs: drop the
	// candidate snapshot so the UI observes fresh data immediately.
	a.app.InvalidateRankingSnapshot()

	a.app.metricsR.AddTestExecuted(result.Working)

	return qr, nil
}

// Enqueue adds one configuration to the test queue.
func (s *TestQueueService) Enqueue(
	fingerprint string,
	protocol string,
	backends []string,
	priority int,
	sourceID string,
) (int64, error) {
	q, err := s.ensureQueue()
	if err != nil {
		return 0, err
	}
	return q.Enqueue(fingerprint, protocol, backends, priority, sourceID, testqueue.EnqueueDefault)
}

// EnqueueItem is one item in a batch enqueue.
type EnqueueItem struct {
	Fingerprint string   `json:"fingerprint"`
	Protocol    string   `json:"protocol"`
	Backends    []string `json:"backends"`
	Priority    int      `json:"priority"`
	Source      string   `json:"source"`
}

// EnqueueMany adds many fingerprints at once. Returns the number of
// tasks actually enqueued (duplicates are skipped).
func (s *TestQueueService) EnqueueMany(tasks []EnqueueItem) int {
	q, err := s.ensureQueue()
	if err != nil {
		return 0
	}

	count := 0
	for _, t := range tasks {
		_, err := q.Enqueue(t.Fingerprint, t.Protocol, t.Backends, t.Priority, t.Source, testqueue.EnqueueDefault)
		if err == nil {
			count++
		}
	}
	return count
}

// Cancel cancels one queued task.
func (s *TestQueueService) Cancel(taskID int64) {
	q, err := s.ensureQueue()
	if err != nil {
		return
	}
	q.Cancel(taskID)
}

// CancelBySource cancels every queued task from one source.
func (s *TestQueueService) CancelBySource(sourceID string) int {
	q, err := s.ensureQueue()
	if err != nil {
		return 0
	}
	return q.CancelBySource(sourceID)
}

// CancelAll cancels every queued task.
func (s *TestQueueService) CancelAll() int {
	q, err := s.ensureQueue()
	if err != nil {
		return 0
	}
	return q.CancelAll()
}

// SetMode changes the queue's testing mode preset. initMu serializes
// the stop+recreate so a concurrent ensureQueue can't observe a
// half-replaced queue or start a second new queue on top.
func (s *TestQueueService) SetMode(mode testqueue.Mode) {
	s.app.initMu.Lock()
	defer s.app.initMu.Unlock()

	cfg := testqueue.ModeConfig(mode)
	s.app.testQueueCfg = cfg
	old := s.app.testQueue
	adapter := &testerAdapter{app: s.app}
	q := testqueue.New(adapter, cfg)
	q.Start(s.app.ctx)
	// Swap the pointer BEFORE stopping the old queue so a concurrent
	// ensureQueue sees the new one. The old queue's Stop drains its
	// in-flight tasks and exits its workers; if any call was in
	// progress on the old queue, it completes against the old queue
	// (the pointer was already swapped, but the caller holds a
	// reference to the old *Queue).
	s.app.testQueue = q
	if old != nil {
		old.Stop()
	}
}

// Stats returns the current queue statistics.
func (s *TestQueueService) Stats() testqueue.Stats {
	q, err := s.ensureQueue()
	if err != nil {
		return testqueue.Stats{}
	}
	return q.Stats()
}

// Pause suspends task pickup: queued tests stay pending while
// in-flight tests finish (v0.9.7 bulk-testing UX).
func (s *TestQueueService) Pause() error {
	q, err := s.ensureQueue()
	if err != nil {
		return err
	}
	q.Pause()
	return nil
}

// Resume lifts a Pause.
func (s *TestQueueService) Resume() error {
	q, err := s.ensureQueue()
	if err != nil {
		return err
	}
	q.Resume()
	return nil
}

// Paused reports whether the queue is paused.
func (s *TestQueueService) Paused() (bool, error) {
	q, err := s.ensureQueue()
	if err != nil {
		return false, err
	}
	return q.Paused(), nil
}

// Snapshot returns the current pending + in-flight tasks (up to limit).
func (s *TestQueueService) Snapshot(limit int) []testqueue.TaskSnapshot {
	q, err := s.ensureQueue()
	if err != nil {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	return q.Snapshot(limit)
}

// Drain blocks until the queue is empty or 30 minutes elapse.
func (s *TestQueueService) Drain() error {
	q, err := s.ensureQueue()
	if err != nil {
		return err
	}
	drainCtx, cancel := context.WithTimeout(s.app.ctx, 30*time.Minute)
	defer cancel()
	return q.Drain(drainCtx)
}

// TestFilter selects which stored configurations a bulk test covers.
type TestFilter struct {
	// Scope is one of: "selected", "all", "untested", "failed",
	// "working" (retest working).
	Scope string `json:"scope"`

	// Fingerprints is required when Scope == "selected".
	Fingerprints []string `json:"fingerprints,omitempty"`

	// Protocol restricts the batch to one protocol ("" = all).
	Protocol string `json:"protocol,omitempty"`

	// Source restricts the batch to one source id ("" = all).
	Source string `json:"source,omitempty"`

	// Limit bounds the batch (0 = 10000; the queue's duplicate
	// suppression keeps huge batches safe).
	Limit int `json:"limit,omitempty"`

	// Priority enqueued for the batch (user batches get a boost).
	Priority int `json:"priority,omitempty"`
}

// TestBatchResult reports what a bulk test enqueued.
type TestBatchResult struct {
	Enqueued int    `json:"enqueued"`
	Skipped  int    `json:"skipped"`
	Scope    string `json:"scope"`
}

// EnqueueByFilter scans the store and enqueues every matching
// configuration. Bounded, streaming, and safe for tens of thousands
// of records (the queue's duplicate suppression collapses repeats).
func (s *TestQueueService) EnqueueByFilter(filter TestFilter) (TestBatchResult, error) {
	q, err := s.ensureQueue()
	if err != nil {
		return TestBatchResult{}, err
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 10000
	}

	priority := filter.Priority
	if priority <= 0 {
		priority = 400 // user-driven bulk test: above discovery, below single test
	}

	result := TestBatchResult{Scope: filter.Scope}

	want := func(cfg config.Config) bool {
		if filter.Protocol != "" && string(cfg.Type) != filter.Protocol {
			return false
		}

		if filter.Source != "" && cfg.Source != filter.Source {
			return false
		}

		switch filter.Scope {
		case "untested":
			return cfg.TestedAt == 0
		case "failed":
			return cfg.TestedAt > 0 && !cfg.Working
		case "timed_out":
			// v0.9.15: the classified failure reason makes "retry timed
			// out" a REAL scope instead of a synonym for "failed".
			return cfg.TestedAt > 0 && !cfg.Working &&
				strings.Contains(strings.ToLower(cfg.LastFailureReason), "timeout")
		case "working":
			return cfg.TestedAt > 0 && cfg.Working
		default: // "all", "selected"
			return true
		}
	}

	switch filter.Scope {
	case "selected":
		for _, fp := range filter.Fingerprints {
			if result.Enqueued >= limit {
				break
			}

			cfg, err := s.app.storeGetConfig(fp)
			if err != nil {
				result.Skipped++

				continue
			}

			if !want(*cfg) {
				result.Skipped++

				continue
			}

			if _, err := q.Enqueue(fp, string(cfg.Type), nil, priority, cfg.Source, testqueue.EnqueueDefault); err == nil {
				result.Enqueued++
			} else {
				result.Skipped++
			}
		}
	default:
		ctx, cancel := context.WithTimeout(s.app.ctx, 2*time.Minute)
		defer cancel()

		err := s.app.store.Iterate(ctx, func(fp string, raw []byte) error {
			if result.Enqueued >= limit {
				return context.Canceled
			}

			var cfg config.Config
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil
			}

			if !want(cfg) {
				return nil
			}

			if _, err := q.Enqueue(fp, string(cfg.Type), nil, priority, cfg.Source, testqueue.EnqueueDefault); err == nil {
				result.Enqueued++
			} else {
				result.Skipped++
			}

			return nil
		})

		if err != nil && err != context.Canceled {
			return result, err
		}
	}

	s.app.logger.Info("testqueue", "batch_enqueued",
		"bulk test scope=%s enqueued=%d skipped=%d", result.Scope, result.Enqueued, result.Skipped)

	return result, nil
}

// --- Tunnel Service ---

// TunnelService exposes the system-proxy / TUN tunnel modes.
type TunnelService struct {
	app *App
}

// NewTunnelService binds a tunnel service to the app.
func NewTunnelService(a *App) *TunnelService {
	return &TunnelService{app: a}
}

// ensureController returns the app's tunnel controller. initMu
// serializes the lazy init so concurrent UI calls don't create
// duplicate controllers.
func (s *TunnelService) ensureController() *tunnel.Controller {
	s.app.initMu.Lock()
	defer s.app.initMu.Unlock()

	if s.app.tunnelCtrl == nil {
		s.app.tunnelCtrl = tunnel.New()
	}
	return s.app.tunnelCtrl
}

// State returns the current tunnel state.
func (s *TunnelService) State() tunnel.State {
	return s.ensureController().State()
}

// EnableSystemProxy sets the Windows system proxy.
func (s *TunnelService) EnableSystemProxy(host string, port int, asHTTP bool, bypass []string) error {
	return s.ensureController().Enable(s.app.ctx, tunnel.ModeSystemProxy, host, port, tunnel.Options{
		AsHTTP: asHTTP,
		Bypass: bypass,
	})
}

// EnableTUN reports the v0.9.8.6 TUN status: experimental and
// disabled. The method is kept on the service surface so older
// frontends receive the explicit, user-visible error instead of a
// missing-method failure — the controller refuses TUN on every
// platform (tunnel.ErrTunExperimental; see engine/tunnel/
// tun_unavailable.go for why the Wintun backend was removed).
func (s *TunnelService) EnableTUN(host string, port int) error {
	return s.ensureController().Enable(s.app.ctx, tunnel.ModeTUN, host, port, tunnel.Options{})
}

// Disable deactivates the active tunnel mode and restores previous settings.
func (s *TunnelService) Disable() error {
	return s.ensureController().Disable(s.app.ctx)
}

// --- Source Manager (v0.6 extension) ---

// SourceStatsView is the UI projection of one source's stats.
// Re-exported as source.Stats; this alias documents the surface
// stable across future Source struct growth.
type SourceStatsView = source.Stats

// SourceStatsList returns the statistics view of all sources.
func (s *SourceService) SourceStatsList() []SourceStatsView {
	s.app.mu.RLock()
	defer s.app.mu.RUnlock()

	out := make([]SourceStatsView, 0, len(s.app.sources))
	for _, src := range s.app.sources {
		out = append(out, src.Stats())
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})

	return out
}

// UpdateSourceMetadata lets the user edit a source's metadata
// (display name, priority, region, protocol hints, refresh interval).
func (s *SourceService) UpdateSourceMetadata(id string, metadata SourceMetadataUpdate) error {
	s.app.mu.Lock()

	found := false
	for i := range s.app.sources {
		if s.app.sources[i].ID == id {
			if metadata.Name != "" {
				s.app.sources[i].Name = metadata.Name
			}
			if metadata.Priority > 0 {
				s.app.sources[i].Priority = metadata.Priority
			}
			if metadata.Region != "" {
				s.app.sources[i].Region = metadata.Region
			}
			if metadata.Format != "" {
				s.app.sources[i].Format = metadata.Format
			}
			if len(metadata.ProtocolHints) > 0 {
				s.app.sources[i].ProtocolHints = metadata.ProtocolHints
			}
			if metadata.RefreshInterval > 0 {
				s.app.sources[i].RefreshInterval = metadata.RefreshInterval
			}
			found = true
			break
		}
	}

	s.app.mu.Unlock()

	if !found {
		return fmt.Errorf("app: source %q not found", id)
	}

	return s.app.saveSources()
}

// SourceMetadataUpdate carries editable source metadata fields.
type SourceMetadataUpdate struct {
	Name            string        `json:"name,omitempty"`
	Priority        int           `json:"priority,omitempty"`
	Region          string        `json:"region,omitempty"`
	Format          string        `json:"format,omitempty"`
	ProtocolHints   []string      `json:"protocol_hints,omitempty"`
	RefreshInterval time.Duration `json:"refresh_interval,omitempty"`
}
