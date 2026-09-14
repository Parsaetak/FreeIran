// Package coremgr implements the Managed Core Manager: install,
// discover, inspect, verify, update, rollback, enable/disable, remove,
// health-check and version reporting for the three protocol cores
// FreeIran depends on (Xray, V2Ray/V2Fly, sing-box).
//
// Design goals (per project specification):
//
//  1. Never overwrite a working executable in place. Updates land in
//     a staging directory, are validated, then atomically activated.
//  2. Update pipeline: download → verify checksum → unpack → validate
//     version → validate executable → smoke test → atomic activate →
//     retain rollback → mark healthy. On any step failing the staged
//     files are removed and the previous healthy version is preserved.
//  3. Stable and prerelease channels are separate; users never auto-
//     install a prerelease unless they explicitly opt in.
//  4. Official upstream release APIs only. No third-party mirrors. No
//     execution of downloaded scripts. Checksum verification is
//     mandatory before activation.
//  5. The UI surfaces a clear lifecycle:
//     Not installed → Installing → Installed → Checking →
//     Ready / Broken / Update available
//
// The package is intentionally self-contained: it depends only on the
// standard library, golang.org/x/sys, and the internal/logging +
// engine/errors packages. It does NOT depend on engine/core (the
// protocol-core execution boundary) so it can be tested in isolation.
package coremgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// Subsystem identifies the manager layer in structured errors.
const Subsystem = "coremgr"

// CoreName is the canonical identifier of a managed protocol core.
type CoreName string

const (
	// CoreXray is the Xray-core backend (XTLS / REALITY / Vision / XHTTP).
	CoreXray CoreName = "xray"

	// CoreV2Ray is the V2Fly V2Ray-core backend (classic V4 format).
	CoreV2Ray CoreName = "v2ray"

	// CoreSingBox is the sing-box backend (native JSON dialect).
	CoreSingBox CoreName = "sing-box"
)

// AllCores enumerates every backend the manager can install.
var AllCores = []CoreName{CoreXray, CoreV2Ray, CoreSingBox}

// InstallState is the persisted lifecycle state of one managed core.
type InstallState string

const (
	StateNotInstalled    InstallState = "not_installed"
	StateInstalling      InstallState = "installing"
	StateInstalled       InstallState = "installed"
	StateChecking        InstallState = "checking"
	StateReady           InstallState = "ready"
	StateBroken          InstallState = "broken"
	StateUpdateAvailable InstallState = "update_available"
	StateDisabled        InstallState = "disabled"
)

// Channel selects which release channel to consult for updates.
type Channel string

const (
	ChannelStable     Channel = "stable"
	ChannelPrerelease Channel = "prerelease"
)

// Manifest is the persisted state of one managed core.
type Manifest struct {
	Name             CoreName     `json:"name"`
	State            InstallState `json:"state"`
	Version          string       `json:"version"`
	Channel          Channel      `json:"channel"`
	BinaryPath       string       `json:"binary_path"`
	ChecksumSHA256   string       `json:"checksum_sha256"`
	SourceURL        string       `json:"source_url"`
	ReleaseTag       string       `json:"release_tag"`
	ReleaseDate      time.Time    `json:"release_date"`
	ReleaseURL       string       `json:"release_url"`
	InstalledAt      time.Time    `json:"installed_at"`
	LastChecked      time.Time    `json:"last_checked"`
	LastHealthCheck  time.Time    `json:"last_health_check"`
	LastHealthResult HealthResult `json:"last_health_result"`
	PreviousVersion  string       `json:"previous_version,omitempty"`
	PreviousChecksum string       `json:"previous_checksum,omitempty"`
	PreviousPath     string       `json:"previous_path,omitempty"`
	// FailureReason is the human-readable explanation for why the
	// core is in the Broken state (v0.9.0: every failed state must
	// carry a useful reason, not just a code).
	FailureReason string `json:"failure_reason,omitempty"`
	// FailureStage is the pipeline step that failed (e.g. "download").
	FailureStage string `json:"failure_stage,omitempty"`
	// LatestKnown is the newest upstream version seen by the last
	// update check ("update available" without a re-query).
	LatestKnown string    `json:"latest_known,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// HealthResult records the outcome of a smoke-test against one core.
type HealthResult struct {
	OK               bool      `json:"ok"`
	ExecutableExists bool      `json:"executable_exists"`
	VersionQuery     bool      `json:"version_query"`
	ConfigValidate   bool      `json:"config_validate"`
	SmokeLaunch      bool      `json:"smoke_launch"`
	CleanShutdown    bool      `json:"clean_shutdown"`
	Details          string    `json:"details,omitempty"`
	CheckedAt        time.Time `json:"checked_at"`
}

// Source describes one official upstream release source.
type Source struct {
	Name             CoreName `json:"name"`
	DisplayName      string   `json:"display_name"`
	Repo             string   `json:"repo"`
	ReleaseAPI       string   `json:"release_api"`
	ReleasePage      string   `json:"release_page"`
	AssetPatterns    []string `json:"asset_patterns"`
	VersionProbeArgs []string `json:"version_probe_args"`
	ConfigCheckArgs  []string `json:"config_check_args"`
	RunArgs          []string `json:"run_args"`
	MinVersion       string   `json:"min_version"`
}

// UpdateInfo is the result of checking for updates.
type UpdateInfo struct {
	Name             CoreName  `json:"name"`
	CurrentVersion   string    `json:"current_version"`
	LatestVersion    string    `json:"latest_version"`
	LatestPrerelease string    `json:"latest_prerelease,omitempty"`
	UpdateAvailable  bool      `json:"update_available"`
	ReleaseTag       string    `json:"release_tag,omitempty"`
	ReleaseDate      time.Time `json:"release_date"`
	ReleaseURL       string    `json:"release_url"`
	ChangelogURL     string    `json:"changelog_url,omitempty"`
	AssetURL         string    `json:"asset_url"`
	AssetSize        int64     `json:"asset_size"`
	CheckedAt        time.Time `json:"checked_at"`
	Err              string    `json:"err,omitempty"`
}

// Manager owns the lifecycle of every managed protocol core.
type Manager struct {
	rootDir    string
	runtimeDir string
	sources    map[CoreName]Source
	httpClient HTTPDoer
	logger     *logging.Logger

	mu        sync.RWMutex
	manifests map[CoreName]*Manifest
	coreMu    map[CoreName]*sync.Mutex

	platform Platform
}

// Platform describes the target OS/arch pair for asset selection.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// DefaultPlatform returns the host platform.
func DefaultPlatform() Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// Options configures the Manager.
type Options struct {
	RootDir string

	// RuntimeDir is the workspace runtime directory (v0.9.2). Short-
	// lived validation/smoke directories are created here instead of
	// the system temp root, so no FreeIran state is ever split across
	// %TEMP% / XDG tmp. Empty = fall back to the system temp root.
	RuntimeDir string

	Sources    map[CoreName]Source
	HTTPClient HTTPDoer
	Logger     *logging.Logger
	Platform   Platform
}

// HTTPDoer is the minimal HTTP interface the manager needs.
// In production it wraps *http.Client; tests inject a fake.
type HTTPDoer interface {
	Do(url string) (*HTTPResponse, error)
}

// HTTPResponse is the response shape returned by HTTPDoer.Do.
type HTTPResponse struct {
	StatusCode int
	Body       []byte
	Headers    map[string]string
}

// ErrAlreadyInstalling is returned when an install is already in flight.
var ErrAlreadyInstalling = errors.New("coremgr: install already in progress")

// ErrNoRollbackTarget is returned when rollback has no target.
var ErrNoRollbackTarget = errors.New("coremgr: no rollback target retained")

// ErrUnsupportedPlatform is returned when no release asset matches.
var ErrUnsupportedPlatform = errors.New("coremgr: no asset matches this platform")

// New constructs a Manager. The root directory is created if missing.
func New(opts Options) (*Manager, error) {
	if opts.RootDir == "" {
		return nil, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "new", "root directory is empty")
	}

	sources := opts.Sources
	if sources == nil {
		sources = DefaultSources()
	}

	if opts.Platform.OS == "" || opts.Platform.Arch == "" {
		opts.Platform = DefaultPlatform()
	}

	m := &Manager{
		rootDir:    opts.RootDir,
		runtimeDir: opts.RuntimeDir,
		sources:    sources,
		httpClient: opts.HTTPClient,
		logger:     opts.Logger,
		manifests:  make(map[CoreName]*Manifest, len(AllCores)),
		coreMu:     make(map[CoreName]*sync.Mutex, len(AllCores)),
		platform:   opts.Platform,
	}

	for _, name := range AllCores {
		m.coreMu[name] = &sync.Mutex{}
	}

	if err := os.MkdirAll(opts.RootDir, 0o700); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "new", "create %s", opts.RootDir)
	}

	for _, name := range AllCores {
		if mf, err := m.loadManifest(name); err == nil && mf != nil {
			m.manifests[name] = mf
		}
	}

	if m.logger == nil {
		m.logger = logging.Global()
	}
	if m.logger == nil {
		// Tests may run without a global logger set. Use a no-op
		// logger so the manager's lifecycle events are not lost in
		// production but tests do not panic.
		dir := opts.RuntimeDir
		if dir == "" {
			dir = os.TempDir()
		}

		m.logger, _ = logging.Open(logging.Options{Dir: dir, Name: "coremgr-test.log"})
	}

	m.logger.Info(Subsystem, "manager_init",
		"managed cores root=%s platform=%s/%s",
		opts.RootDir, opts.Platform.OS, opts.Platform.Arch)

	return m, nil
}

// RootDir returns the managed cores root directory.
func (m *Manager) RootDir() string { return m.rootDir }

// RuntimeDir returns the directory used for short-lived helper
// directories (workspace runtime when configured, system temp
// otherwise).
func (m *Manager) RuntimeDir() string { return m.tempRoot() }

// tempRoot resolves the runtime directory for temporary helper
// folders, creating it when it is workspace-provided.
func (m *Manager) tempRoot() string {
	if m.runtimeDir == "" {
		return os.TempDir()
	}

	if err := os.MkdirAll(m.runtimeDir, 0o700); err != nil {
		return os.TempDir()
	}

	return m.runtimeDir
}

// CoreDir returns the per-core directory.
func (m *Manager) CoreDir(name CoreName) string {
	return filepath.Join(m.rootDir, string(name))
}

// BinDir returns the per-core binary directory.
func (m *Manager) BinDir(name CoreName) string {
	return filepath.Join(m.CoreDir(name), "bin")
}

// StagingDir returns the per-core staging directory.
func (m *Manager) StagingDir(name CoreName) string {
	return filepath.Join(m.CoreDir(name), "staging")
}

// CleanStaleStaging removes stale core-install staging trees and
// abandoned failed-download artifacts older than maxAge. Staging is
// reconstructable by definition: a live install re-creates it, and a
// successful install never leaves content behind. Old rollback
// binaries, manifests and bin/ executables are NEVER touched (they
// are live core metadata). The walk is bounded to the per-core
// staging directories plus top-level download leftovers, so the scan
// stays cheap. Returns the bytes reclaimed.
func (m *Manager) CleanStaleStaging(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		maxAge = 7 * 24 * time.Hour
	}

	var reclaimed int64

	for _, name := range AllCores {
		if err := ctx.Err(); err != nil {
			return reclaimed, err
		}

		staging := m.StagingDir(name)

		entries, err := os.ReadDir(staging)
		if err != nil {
			continue // no staging tree for this core
		}

		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return reclaimed, err
			}

			path := filepath.Join(staging, entry.Name())

			info, err := entry.Info()
			if err != nil {
				continue
			}

			if time.Since(info.ModTime()) < maxAge {
				continue
			}

			size := dirSize(path)

			if err := os.RemoveAll(path); err == nil {
				reclaimed += size
			}
		}

		// Also drop the staging directory itself when empty and old.
		if info, err := os.Stat(staging); err == nil &&
			time.Since(info.ModTime()) >= maxAge {
			entries, err := os.ReadDir(staging)
			if err == nil && len(entries) == 0 {
				_ = os.Remove(staging)
			}
		}
	}

	return reclaimed, nil
}

// dirSize sums the file sizes under path (bounded walk used for
// reclaimed-bytes accounting).
func dirSize(path string) int64 {
	var total int64

	_ = filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if info, err := entry.Info(); err == nil && entry.Type().IsRegular() {
			total += info.Size()
		}

		return nil
	})

	return total
}

// ManifestPath returns the per-core manifest file path.
func (m *Manager) ManifestPath(name CoreName) string {
	return filepath.Join(m.CoreDir(name), "manifest.json")
}

// BinaryPath returns the path to the active executable for a core.
func (m *Manager) BinaryPath(name CoreName) string {
	return filepath.Join(m.BinDir(name), executableName(string(name)))
}

// RollbackPath returns the path to the retained rollback executable.
func (m *Manager) RollbackPath(name CoreName) string {
	return filepath.Join(m.BinDir(name), executableName(string(name))+".previous")
}

// executableName returns the platform-correct filename for a core.
func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// lock acquires the per-core mutex.
func (m *Manager) lock(name CoreName) func() {
	mu := m.coreMu[name]
	mu.Lock()
	return mu.Unlock
}

// Info returns the public view of one core's lifecycle state.
func (m *Manager) Info(name CoreName) (Manifest, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	mf, ok := m.manifests[name]
	if !ok {
		return Manifest{
			Name:    name,
			State:   StateNotInstalled,
			Channel: ChannelStable,
		}, false
	}
	snapshot := *mf
	return snapshot, true
}

// All returns the lifecycle state of every managed core.
func (m *Manager) All() []Manifest {
	out := make([]Manifest, 0, len(AllCores))
	for _, name := range AllCores {
		mf, _ := m.Info(name)
		out = append(out, mf)
	}
	return out
}

// SetChannel changes the release channel for a core.
func (m *Manager) SetChannel(name CoreName, ch Channel) error {
	unlock := m.lock(name)
	defer unlock()

	m.mu.Lock()
	mf := m.manifestOrCreateLocked(name)
	mf.Channel = ch
	mf.UpdatedAt = time.Now().UTC()
	m.mu.Unlock()

	return m.persist(name)
}

// manifestOrCreateLocked returns the in-memory manifest, creating an
// empty one if missing. Caller MUST hold m.mu (write).
func (m *Manager) manifestOrCreateLocked(name CoreName) *Manifest {
	if mf, ok := m.manifests[name]; ok {
		return mf
	}
	mf := &Manifest{
		Name:    name,
		State:   StateNotInstalled,
		Channel: ChannelStable,
	}
	m.manifests[name] = mf
	return mf
}

// snapshotManifest returns a value copy of the manifest. Safe to call
// concurrently with mutations (the copy is taken under m.mu.RLock).
func (m *Manager) snapshotManifest(name CoreName) (Manifest, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mf, ok := m.manifests[name]
	if !ok {
		return Manifest{Name: name, State: StateNotInstalled, Channel: ChannelStable}, false
	}
	return *mf, true
}

// updateManifest applies fn to the manifest under m.mu.Lock, then
// persists. fn must not acquire any lock that could deadlock with
// m.mu (in particular, fn must not call m.lock or m.persist). Use
// this for short read-modify-write sequences. For long operations
// (download, smoke test), snapshot first, do the work outside the
// lock, then call updateManifest to write back.
func (m *Manager) updateManifest(name CoreName, fn func(mf *Manifest)) error {
	m.mu.Lock()
	mf := m.manifestOrCreateLocked(name)
	fn(mf)
	m.mu.Unlock()
	return m.persist(name)
}

// setState updates the in-memory state and persists the manifest.
func (m *Manager) setState(name CoreName, state InstallState) error {
	return m.updateManifest(name, func(mf *Manifest) {
		mf.State = state
		mf.UpdatedAt = time.Now().UTC()
	})
}

// persist writes the manifest to disk atomically. Takes a consistent
// snapshot under m.mu.RLock before marshalling so concurrent
// mutations don't produce a torn write.
func (m *Manager) persist(name CoreName) error {
	m.mu.RLock()
	mf, ok := m.manifests[name]
	if !ok {
		m.mu.RUnlock()
		return nil
	}
	snapshot := *mf
	m.mu.RUnlock()

	if err := os.MkdirAll(m.CoreDir(name), 0o700); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "persist", "mkdir %s", m.CoreDir(name))
	}

	raw, err := jsonMarshalIndent(&snapshot)
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindConfiguration,
			Subsystem, "persist", "encode manifest")
	}

	return atomicWrite(m.ManifestPath(name), raw, 0o600)
}

// loadManifest reads the manifest from disk.
func (m *Manager) loadManifest(name CoreName) (*Manifest, error) {
	raw, err := os.ReadFile(m.ManifestPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "load", "read manifest %s", name)
	}

	var mf Manifest
	if err := jsonUnmarshal(raw, &mf); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindConfiguration,
			Subsystem, "load", "decode manifest %s", name)
	}

	mf.BinaryPath = m.BinaryPath(name)
	if mf.PreviousPath != "" {
		mf.PreviousPath = m.RollbackPath(name)
	}
	return &mf, nil
}

// Remove uninstalls a core.
func (m *Manager) Remove(ctx context.Context, name CoreName) error {
	unlock := m.lock(name)
	defer unlock()

	if err := os.RemoveAll(m.CoreDir(name)); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "remove", "rm -rf %s", m.CoreDir(name))
	}

	m.mu.Lock()
	delete(m.manifests, name)
	m.mu.Unlock()

	m.logger.Info(Subsystem, "core_removed", "core %s removed", name)
	return nil
}

// Disable marks a core as disabled.
func (m *Manager) Disable(name CoreName) error {
	unlock := m.lock(name)
	defer unlock()
	return m.setState(name, StateDisabled)
}

// Reinstall removes every local artifact of a core and performs a
// fresh install from the official upstream release. It is the
// strongest recovery action after a plain repair is not enough.
func (m *Manager) Reinstall(ctx context.Context, name CoreName) error {
	if err := m.Remove(ctx, name); err != nil {
		return err
	}
	return m.Install(ctx, name)
}

// ExplainFailure returns a human-readable reason for a core's current
// failure state. Empty when the core is healthy.
func (m *Manager) ExplainFailure(name CoreName) string {
	snap, ok := m.snapshotManifest(name)
	if !ok {
		return ""
	}

	switch snap.State {
	case StateBroken:
		if snap.FailureReason != "" {
			return snap.FailureReason
		}

		if snap.LastHealthResult.Details != "" {
			return snap.LastHealthResult.Details
		}

		return "the core failed its last verification"
	case StateDisabled:
		return "the core was disabled by the user"
	default:
		return ""
	}
}

// Enable re-activates a disabled core.
func (m *Manager) Enable(ctx context.Context, name CoreName) error {
	unlock := m.lock(name)
	defer unlock()

	// Snapshot under RLock to check BinaryPath without holding the
	// lock during os.Stat.
	snap, _ := m.snapshotManifest(name)
	binaryPath := snap.BinaryPath
	if binaryPath == "" {
		binaryPath = m.BinaryPath(name)
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return m.setState(name, StateBroken)
	}
	return m.updateManifest(name, func(mf *Manifest) {
		if mf.BinaryPath == "" {
			mf.BinaryPath = m.BinaryPath(name)
		}
		// The binary survived, but its health is unknown until the
		// next check: restore the Installed state (not Ready) and
		// clear stale failure diagnostics.
		mf.State = StateInstalled
		mf.FailureReason = ""
		mf.FailureStage = ""
		mf.UpdatedAt = time.Now().UTC()
	})
}

// SortByName orders a slice of manifests by core name.
func SortByName(mfs []Manifest) {
	sort.Slice(mfs, func(i, j int) bool {
		return mfs[i].Name < mfs[j].Name
	})
}

// String renders the manager state for diagnostics.
func (m *Manager) String() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	parts := make([]string, 0, len(m.manifests))
	for _, name := range AllCores {
		mf, ok := m.manifests[name]
		if !ok {
			parts = append(parts, fmt.Sprintf("%s=not_installed", name))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s/%s", name, mf.State, mf.Version))
	}
	return "coremgr[" + joinStrings(parts, ", ") + "]"
}

func joinStrings(values []string, sep string) string {
	result := ""
	for i, v := range values {
		if i > 0 {
			result += sep
		}
		result += v
	}
	return result
}
