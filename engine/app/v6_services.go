package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/coremgr"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/testqueue"
	"github.com/Parsaetak/FreeIran/engine/tunnel"
)

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

// ensureCoreMgr returns the app's core manager, initializing it on
// first use. The manager is created lazily so a headless test setup
// without a writable AppData directory still boots.
func (s *CoreService) ensureCoreMgr() (*coremgr.Manager, error) {
	if s.app.coreMgr != nil {
		return s.app.coreMgr, nil
	}

	mgr, err := coremgr.New(coremgr.Options{
		RootDir: s.app.layout.Cores,
		Logger:  s.app.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("app: init core manager: %w", err)
	}

	s.app.coreMgr = mgr
	return mgr, nil
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
// core. This is a long-running operation.
func (s *CoreService) Install(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}
	return mgr.Install(s.app.ctx, name)
}

// Uninstall removes one core and its manifest.
func (s *CoreService) Uninstall(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}
	return mgr.Remove(s.app.ctx, name)
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
	return mgr.Rollback(s.app.ctx, name)
}

// Repair tries to fix a broken core.
func (s *CoreService) Repair(name coremgr.CoreName) error {
	mgr, err := s.ensureCoreMgr()
	if err != nil {
		return err
	}
	return mgr.Repair(s.app.ctx, name)
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
	return mgr.Enable(s.app.ctx, name)
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

// ensureQueue returns the app's test queue, initializing it lazily.
func (s *TestQueueService) ensureQueue() (*testqueue.Queue, error) {
	if s.app.testQueue != nil {
		return s.app.testQueue, nil
	}

	cfg := testqueue.DefaultConfig()
	adapter := &testerAdapter{app: s.app}
	q := testqueue.New(adapter, cfg)
	q.Start(s.app.ctx)

	s.app.testQueue = q
	return q, nil
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
//  2. resolves the candidate backends (the first available +
//     healthy backend);
//  3. runs the existing tester.Test against the config + backend;
//  4. maps the tester.Result to a testqueue.Result.
func (a *testerAdapter) Test(ctx context.Context, fingerprint string, backends []string) (testqueue.Result, error) {
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

	// 2. Run the existing tester.
	result := a.app.tester.Test(ctx, cfg)
	return testqueue.Result{
		Working:   result.Working,
		Latency:   result.Latency,
		TestedAt:  result.TestedAt,
		LastError: result.LastError,
	}, nil
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

// SetMode changes the queue's testing mode preset.
func (s *TestQueueService) SetMode(mode testqueue.Mode) {
	cfg := testqueue.ModeConfig(mode)
	s.app.testQueueCfg = cfg
	if s.app.testQueue != nil {
		s.app.testQueue.Stop()
	}
	adapter := &testerAdapter{app: s.app}
	q := testqueue.New(adapter, cfg)
	q.Start(s.app.ctx)
	s.app.testQueue = q
}

// Stats returns the current queue statistics.
func (s *TestQueueService) Stats() testqueue.Stats {
	q, err := s.ensureQueue()
	if err != nil {
		return testqueue.Stats{}
	}
	return q.Stats()
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

// --- Tunnel Service ---

// TunnelService exposes the system-proxy / TUN tunnel modes.
type TunnelService struct {
	app *App
}

// NewTunnelService binds a tunnel service to the app.
func NewTunnelService(a *App) *TunnelService {
	return &TunnelService{app: a}
}

// ensureController returns the app's tunnel controller.
func (s *TunnelService) ensureController() *tunnel.Controller {
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

// EnableTUN enables TUN mode. Requires elevation on Windows.
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
