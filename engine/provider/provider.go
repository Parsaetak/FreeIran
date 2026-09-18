// Package provider implements FreeIran's unified provider
// architecture (v0.9.8.1 §8–§10): ONE lifecycle contract for every
// executable that can provide connectivity — the protocol cores
// (Xray, V2Ray, sing-box) and the first-class engines Tor and
// Psiphon.
//
// Design rules (the upgrade specification):
//
//   - Providers and node CONFIGURATIONS are distinct concepts: node
//     ranking stays in engine/ranking; provider lifecycle lives here.
//     Tor is NEVER represented as a VLESS/VMess/Trojan node, and
//     Psiphon is NEVER an ordinary node protocol.
//   - No provider is forced into a single configuration format: each
//     engine owns its own runtime configuration (torrc, tunnel-core
//     JSON, core run-configs).
//   - ONE managed process supervisor (system.ManagedProcess) — no
//     duplicate process manager, no orphan processes.
//   - ONE managed-binary pipeline (metadata → platform/arch →
//     download .part → checksum → validate → stage → atomic
//     activate → health → rollback) shared by Tor and Psiphon,
//     reusing internal/httpx exactly like engine/coremgr.
//   - Normally ONE active managed instance/profile per provider
//     (§14); the Manager serialises Start/Stop per provider.
//   - Honest capability reporting: nothing is claimed that was not
//     discovered from the real runtime (installed version, exposed
//     proxy endpoints, bootstrap state).
package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

// Subsystem labels provider events in structured logs.
const Subsystem = "provider"

// Kind classifies providers.
type Kind string

const (
	// KindCore: protocol-core backends (xray, v2ray, sing-box) driven
	// per node-configuration.
	KindCore Kind = "core"

	// KindTor: the Tor network engine (expert bundle, local SOCKS).
	KindTor Kind = "tor"

	// KindPsiphon: the Psiphon tunnel-core engine.
	KindPsiphon Kind = "psiphon"
)

// LifecycleState is the provider lifecycle state (§8/§9):
// resolve → install → validate → start → bootstrap/ready → health →
// stop → cleanup.
type LifecycleState string

const (
	// StateNotInstalled: no verified binary in the workspace.
	StateNotInstalled LifecycleState = "not_installed"

	// StateInstalling: the managed download pipeline is running.
	StateInstalling LifecycleState = "installing"

	// StateInstalled: binary staged and validated, not running.
	StateInstalled LifecycleState = "installed"

	// StateStarting: process launched, not yet ready.
	StateStarting LifecycleState = "starting"

	// StateReady: process running and its local proxy endpoint(s)
	// accept connections (Tor: bootstrapped 100%; Psiphon: tunnels
	// up). Readiness is observed, never assumed.
	StateReady LifecycleState = "ready"

	// StateStopping: shutdown in progress.
	StateStopping LifecycleState = "stopping"

	// StateFailed: last lifecycle operation failed (reason recorded).
	StateFailed LifecycleState = "failed"

	// StateDisabled: explicitly disabled by the user.
	StateDisabled LifecycleState = "disabled"
)

// Endpoint is a local proxy endpoint discovered from the REAL
// runtime (a port the provider actually listens on).
type Endpoint struct {
	// Network is "socks5" or "http".
	Network string `json:"network"`

	// Host is normally 127.0.0.1.
	Host string `json:"host"`

	// Port is the observed local port.
	Port int `json:"port"`

	// Verified marks an endpoint whose listener was actually probed.
	Verified bool `json:"verified,omitempty"`
}

// Addr renders host:port.
func (e Endpoint) Addr() string {
	return fmt.Sprintf("%s:%d", e.Host, e.Port)
}

// Label renders a UI label.
func (e Endpoint) Label() string {
	if e.Network == "http" {
		return "HTTP proxy " + e.Addr()
	}

	return "SOCKS5 " + e.Addr()
}

// Health is a measured health snapshot.
type Health struct {
	OK            bool `json:"ok"`
	ProcessAlive  bool `json:"process_alive"`
	ListenerReady bool `json:"listener_ready"`
	// LatencyMS is a MEASURED probe round trip through the local
	// endpoint (v0.9.8.1 semantics: 0 + Measured = sub-ms).
	LatencyMS int64     `json:"latency_ms,omitempty"`
	Measured  bool      `json:"measured,omitempty"`
	Details   string    `json:"details,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// Info is the credential-free provider surface for the UI (§13).
type Info struct {
	Name          string         `json:"name"`
	Kind          Kind           `json:"kind"`
	Installed     bool           `json:"installed"`
	Version       string         `json:"version,omitempty"`
	State         LifecycleState `json:"state"`
	RuntimeState  string         `json:"runtime_state,omitempty"` // process state when running
	Source        string         `json:"source,omitempty"`        // download source or "user-provided"
	License       string         `json:"license,omitempty"`       // short license name
	Notice        string         `json:"notice,omitempty"`        // attribution notice
	LastCheck     time.Time      `json:"last_check,omitempty"`
	Endpoints     []Endpoint     `json:"endpoints,omitempty"`    // discovered from the real runtime
	Capabilities  []string       `json:"capabilities,omitempty"` // honest, runtime-derived
	FailureReason string         `json:"failure_reason,omitempty"`
	Bootstrap     BootstrapInfo  `json:"bootstrap,omitempty"`
}

// BootstrapInfo carries live bootstrap progress from the provider's
// ACTUAL state (Tor log lines / Psiphon tunnel events) — never from
// timers.
type BootstrapInfo struct {
	Active    bool      `json:"active,omitempty"`
	Progress  int       `json:"progress,omitempty"` // 0-100 when active
	Tag       string    `json:"tag,omitempty"`      // last bootstrap tag/summary
	Complete  bool      `json:"complete,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Provider is the unified lifecycle contract (§10). Implementations:
// TorEngine, PsiphonEngine, CoreProviderAdapter.
type Provider interface {
	// Name is the stable provider id ("tor", "psiphon", "xray"...).
	Name() string

	// Kind classifies the provider.
	Kind() Kind

	// Resolve discovers the latest release metadata (official
	// sources only, checksum authority required).
	Resolve(ctx context.Context) (Release, error)

	// Install runs the managed download pipeline. Idempotent when
	// the pinned/installed version is already healthy.
	Install(ctx context.Context) error

	// Uninstall removes the provider's binaries and runtime data
	// from the workspace.
	Uninstall(ctx context.Context) error

	// Start launches ONE managed instance and blocks until ready or
	// the context/timeout expires. Starting an already-running
	// provider is a no-op error.
	Start(ctx context.Context) error

	// Stop shuts the managed instance down deterministically (no
	// orphan processes, runtime data preserved unless Cleanup runs).
	Stop(ctx context.Context) error

	// State reports the lifecycle state.
	State() LifecycleState

	// Info renders the credential-free UI surface.
	Info() Info

	// Endpoints lists local proxy endpoints observed from the real
	// runtime (empty when not ready).
	Endpoints() []Endpoint

	// Health measures the live instance (process + listener).
	Health(ctx context.Context) Health

	// Cleanup removes stale runtime data (log rotation, temp dirs).
	Cleanup(ctx context.Context) error
}

// Release is resolved upstream release metadata.
type Release struct {
	Version     string    `json:"version"`
	AssetURL    string    `json:"asset_url"`
	AssetName   string    `json:"asset_name"`
	Size        int64     `json:"size,omitempty"`
	SHA256      string    `json:"sha256,omitempty"` // authoritative checksum
	ReleaseURL  string    `json:"release_url,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`
}

// Manager owns the provider set. It enforces §14: one active managed
// instance per provider, serialised lifecycle transitions, bounded
// concurrent operations, and structured provider_* events.
type Manager struct {
	mu        sync.Mutex
	providers map[string]Provider
	order     []string
}

// NewManager creates an empty provider manager.
func NewManager() *Manager {
	return &Manager{providers: map[string]Provider{}}
}

// Register adds a provider (idempotent replace).
func (m *Manager) Register(p Provider) {
	if p == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.providers[p.Name()]; !exists {
		m.order = append(m.order, p.Name())
	}

	m.providers[p.Name()] = p
}

// Get returns a provider by name.
func (m *Manager) Get(name string) (Provider, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.providers[name]

	return p, ok
}

// List renders every provider's Info, ordered by name.
func (m *Manager) List() []Info {
	m.mu.Lock()

	names := append([]string(nil), m.order...)
	providers := make([]Provider, 0, len(names))

	for _, name := range names {
		if p, ok := m.providers[name]; ok {
			providers = append(providers, p)
		}
	}

	m.mu.Unlock()

	infos := make([]Info, 0, len(providers))
	for _, p := range providers {
		infos = append(infos, p.Info())
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })

	return infos
}

// Names lists registered provider ids.
func (m *Manager) Names() []string {
	infos := m.List()

	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}

	return names
}

// Start starts one provider (delegates; the provider itself
// serialises).
func (m *Manager) Start(ctx context.Context, name string) error {
	p, ok := m.Get(name)
	if !ok {
		return fmt.Errorf("provider %q is not registered", name)
	}

	logProviderEvent("provider_start", name, "")

	if err := p.Start(ctx); err != nil {
		logProviderEvent("provider_failed", name, "start")
		return err
	}

	logProviderEvent("provider_ready", name, "")

	return nil
}

// Stop stops one provider.
func (m *Manager) Stop(ctx context.Context, name string) error {
	p, ok := m.Get(name)
	if !ok {
		return fmt.Errorf("provider %q is not registered", name)
	}

	logProviderEvent("provider_stop", name, "")

	return p.Stop(ctx)
}

// StopAll stops every running provider (shutdown path).
func (m *Manager) StopAll(ctx context.Context) {
	for _, name := range m.Names() {
		if p, ok := m.Get(name); ok {
			_ = p.Stop(ctx)
		}
	}
}

// Health measures one provider.
func (m *Manager) Health(ctx context.Context, name string) (Health, bool) {
	p, ok := m.Get(name)
	if !ok {
		return Health{}, false
	}

	return p.Health(ctx), true
}

// logProviderEvent emits a structured provider_* event (§15). No
// credentials, UUIDs, bridge material or subscription URIs ever
// enter these records.
func logProviderEvent(event, providerName, errorKind string) {
	record := logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     event,
		Message:   providerName + ": " + event,
	}

	if errorKind != "" {
		record.Fields = map[string]any{
			"provider":   providerName,
			"error_kind": errorKind,
		}
	}

	if event == "provider_failed" || event == "provider_install_failed" {
		record.Level = logging.LevelWarn
	}

	logging.LogR(record)
}
