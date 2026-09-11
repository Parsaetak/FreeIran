package core

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/system"
)

// BackendStatus is the availability state of one backend runtime.
type BackendStatus string

const (
	// StatusAvailable: executable discovered and responsive.
	StatusAvailable BackendStatus = "available"

	// StatusMissing: no executable found in any search location.
	StatusMissing BackendStatus = "missing"

	// StatusInvalid: executable found but unusable (version probe
	// failed repeatedly or launch refused).
	StatusInvalid BackendStatus = "invalid"
)

// BackendInfo is the registry's public view of one backend: identity,
// availability, version, executable path, capabilities and priority.
type BackendInfo struct {
	Name         string        `json:"name"`
	Status       BackendStatus `json:"status"`
	Version      string        `json:"version,omitempty"`
	Path         string        `json:"path,omitempty"`
	Priority     int           `json:"priority"`
	Capabilities Capabilities  `json:"-"`
	Summary      string        `json:"summary,omitempty"`
	LastCheck    time.Time     `json:"last_check,omitempty"`
	Note         string        `json:"note,omitempty"`
}

// Registry manages the set of registered protocol-core backends and
// their runtime availability. It is the only place where backend
// selection happens: callers ask the registry, never a scattered
// if/else chain.
type Registry struct {
	locator *system.CoreLocator

	mu       sync.RWMutex
	backends map[string]*registeredBackend
}

type registeredBackend struct {
	core     Core
	priority int
	status   BackendStatus
	version  string
	path     string
	note     string
	check    time.Time
}

// NewRegistry creates an empty registry bound to an executable
// locator. The locator may be nil (tests with binary-path overrides);
// discovery then reports every backend as missing.
func NewRegistry(locator *system.CoreLocator) *Registry {
	return &Registry{
		locator:  locator,
		backends: make(map[string]*registeredBackend),
	}
}

// Register adds or replaces a backend. Lower priority values win
// during selection (0 = highest). Registration is cheap and
// side-effect free; executable discovery happens on Refresh.
func (r *Registry) Register(c Core, priority int) error {
	if c == nil {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "register", "backend is nil")
	}

	name := c.Name()

	if name == "" {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "register", "backend name is empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.backends[name] = &registeredBackend{
		core:     c,
		priority: priority,
		status:   StatusMissing,
	}

	return nil
}

// Get returns a registered backend by name.
func (r *Registry) Get(name string) (Core, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.backends[name]
	if !ok {
		return nil, false
	}

	return entry.core, true
}

// Refresh re-runs executable discovery for every registered backend
// and updates availability, path and version information. Missing or
// invalid binaries are reported through BackendInfo, never as errors:
// a desktop app must boot with zero cores installed.
func (r *Registry) Refresh(ctx context.Context) {
	if r == nil {
		return
	}

	r.mu.RLock()

	type discovery struct {
		name string
		core Core
	}

	work := make([]discovery, 0, len(r.backends))

	for name, entry := range r.backends {
		work = append(work, discovery{name: name, core: entry.core})
	}

	r.mu.RUnlock()

	for _, item := range work {
		status, version, path, note := r.discover(ctx, item.name)

		r.mu.Lock()

		if entry, ok := r.backends[item.name]; ok {
			entry.status = status
			entry.version = version
			entry.path = path
			entry.note = note
			entry.check = time.Now().UTC()
		}

		r.mu.Unlock()
	}
}

// discover probes one backend's executable. With no locator, or when
// discovery fails, the backend stays missing but remains registered.
func (r *Registry) discover(ctx context.Context, name string) (BackendStatus, string, string, string) {
	if r.locator == nil {
		return StatusMissing, "", "", "no executable locator configured"
	}

	binary, err := r.locator.Discover(ctx, name)
	if err != nil {
		return StatusMissing, "", "", "executable not found in cores directory or PATH"
	}

	if binary.Path == "" {
		return StatusInvalid, "", "", "discovered executable has no path"
	}

	// A missing version string is tolerated (probe forms are tried on
	// Refresh; some builds may not implement any of them), but a core
	// that never produced a version is flagged in diagnostics.
	note := ""
	if binary.Version == "" {
		note = "executable found but version probe returned no output"
	}

	return StatusAvailable, binary.Version, binary.Path, note
}

// Info returns the public view of one backend.
func (r *Registry) Info(name string) (BackendInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.backends[name]
	if !ok {
		return BackendInfo{}, false
	}

	var caps Capabilities

	if provider, isProvider := entry.core.(interface {
		Capabilities() Capabilities
	}); isProvider {
		caps = provider.Capabilities()
	}

	return BackendInfo{
		Name:         name,
		Status:       entry.status,
		Version:      entry.version,
		Path:         entry.path,
		Priority:     entry.priority,
		Capabilities: caps,
		Summary:      caps.Summary(),
		LastCheck:    entry.check,
		Note:         entry.note,
	}, true
}

// Backends returns every registered backend ordered by priority then
// name — the deterministic registry listing surfaced to the UI.
func (r *Registry) Backends() []BackendInfo {
	r.mu.RLock()

	entries := make([]BackendInfo, 0, len(r.backends))

	for name, entry := range r.backends {
		var caps Capabilities

		if provider, ok := entry.core.(interface {
			Capabilities() Capabilities
		}); ok {
			caps = provider.Capabilities()
		}

		entries = append(entries, BackendInfo{
			Name:         name,
			Status:       entry.status,
			Version:      entry.version,
			Path:         entry.path,
			Priority:     entry.priority,
			Capabilities: caps,
			Summary:      caps.Summary(),
			LastCheck:    entry.check,
			Note:         entry.note,
		})
	}

	r.mu.RUnlock()

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Priority != entries[j].Priority {
			return entries[i].Priority < entries[j].Priority
		}

		return entries[i].Name < entries[j].Name
	})

	return entries
}

// Version returns the discovered version string of a backend.
func (r *Registry) Version(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if entry, ok := r.backends[name]; ok {
		return entry.version
	}

	return ""
}

// BinaryPath returns the discovered executable path of a backend.
func (r *Registry) BinaryPath(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if entry, ok := r.backends[name]; ok {
		return entry.path
	}

	return ""
}

// supportsProtocol reports whether any available backend declares the
// protocol — used by probes to advertise support without a full
// configuration.
func (r *Registry) supportsProtocol(t config.Type) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, entry := range r.backends {
		if entry.status != StatusAvailable {
			continue
		}

		if provider, ok := entry.core.(interface {
			Capabilities() Capabilities
		}); ok && provider.Capabilities().SupportsProtocol(t) {
			return true
		}
	}

	return false
}

// String renders the registry state for diagnostics (no secrets are
// involved in backend metadata).
func (r *Registry) String() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.backends))

	for name := range r.backends {
		names = append(names, name)
	}

	sort.Strings(names)

	parts := make([]string, 0, len(names))

	for _, name := range names {
		entry := r.backends[name]
		state := "missing"

		switch entry.status {
		case StatusAvailable:
			state = fmt.Sprintf("available %s", entry.version)
		case StatusInvalid:
			state = "invalid"
		}

		parts = append(parts, name+"="+state)
	}

	return "registry[" + joinStrings(parts, ", ") + "]"
}

func joinStrings(values []string, sep string) string {
	result := ""

	for i, value := range values {
		if i > 0 {
			result += sep
		}

		result += value
	}

	return result
}
