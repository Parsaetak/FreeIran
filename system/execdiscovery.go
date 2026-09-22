package system

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/singleflight"
)

// ExecDiscovery is the ONE authoritative executable-discovery
// mechanism (v0.9.14): every subsystem that needs to find an installed
// engine — the core registry, the core manager's reuse decisions, the
// provider manager (Tor/Psiphon), diagnostics and the UI projections —
// resolves candidates through it. No subsystem independently walks
// PATH or installation directories.
//
// Design invariants (v0.9.14 contract):
//
//   - BOUNDED search: discovery probes a fixed, platform-aware set of
//     known roots and known executable names. A complete-disk recursive
//     walk is prohibited — it would create unacceptable startup cost,
//     privacy implications and unpredictable latency.
//   - DISCOVER → IDENTIFY → VALIDATE → COMPARE: candidates carry
//     origin, ownership and probed version so callers can make an
//     explainable reuse decision (managed-current, system-current,
//     system-newer, system-older-but-working, invalid-local, not-found
//     …) instead of a lossy "installed=true" boolean.
//   - DO NOT RE-PROBE: the version probe (a process spawn) runs once
//     per (path, file-identity). Results are cached under a stable
//     fingerprint (path, size, mtime) with a bounded freshness TTL and
//     explicit invalidation. Concurrent probes for the same identity
//     are deduplicated in flight — one spawn, shared result.
//   - Cache invalidation is precise: file identity change, vanished
//     path, explicit Verify, completed install/update, runtime failure
//     or a configuration change affecting selection. Stale entries
//     never outlive their TTL.

// Origin describes where a discovered executable was found.
type Origin string

const (
	// OriginManaged: a FreeIran workspace-managed location
	// (<workspace>/cores/<core>/bin, <workspace>/providers/<p>).
	OriginManaged Origin = "managed"

	// OriginPath: resolved through the real OS PATH lookup.
	OriginPath Origin = "path"

	// OriginSystem: a known platform installation location
	// (Program Files / LocalAppData on Windows, /usr/local/...,
	// /opt, ~/.local on Linux/macOS).
	OriginSystem Origin = "system"

	// OriginUser: a user-supplied binary adopted through the
	// copy-not-move adoption path (Psiphon user binaries).
	OriginUser Origin = "user"
)

// Ownership distinguishes FreeIran-owned binaries from external ones.
// FreeIran never deletes, renames or overwrites an external binary;
// it only records a reference.
type Ownership string

const (
	// OwnershipManaged: the binary lives inside the FreeIran
	// workspace and is managed (installed, updated, removed) by it.
	OwnershipManaged Ownership = "managed"

	// OwnershipExternal: the binary belongs to the user or to another
	// installation; FreeIran only references it.
	OwnershipExternal Ownership = "external"
)

// String renders the origin for diagnostics and UI surfaces.
func (o Origin) String() string { return string(o) }

// String renders the ownership for diagnostics and UI surfaces.
func (o Ownership) String() string { return string(o) }

// FileIdentity is the stable fingerprint used to cache probe results.
// When path, size and mtime are all unchanged the previous probe
// result is reused without spawning a new process.
type FileIdentity struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// fileIdentityOf stats path and returns its identity. The bool result
// is false when the file does not exist or cannot be stated.
func fileIdentityOf(path string) (FileIdentity, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return FileIdentity{}, false
	}

	return FileIdentity{
		Path:    path,
		Size:    info.Size(),
		ModTime: info.ModTime().UTC(),
	}, true
}

// ExecCandidate is one discovered executable installation of a known
// engine, with enough identity to make a deterministic, explainable
// reuse decision.
type ExecCandidate struct {
	// Engine is the engine identity the candidate was discovered for
	// (e.g. "xray", "tor").
	Engine string `json:"engine"`

	// Path is the canonical absolute executable path.
	Path string `json:"path"`

	// Origin is where the candidate was found (managed, path,
	// system, user).
	Origin Origin `json:"origin"`

	// Ownership distinguishes FreeIran-managed files from external
	// ones (never deleted or modified by FreeIran).
	Ownership Ownership `json:"ownership"`

	// Version is the probed version line ("" when the probe produced
	// no output — the candidate still exists but is unvalidated).
	Version string `json:"version,omitempty"`

	// Size and ModTime are the file identity the probe was cached
	// under.
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`

	// LastSeen is when this candidate was last discovered/validated.
	LastSeen time.Time `json:"last_seen"`
}

// Validated reports whether the candidate produced a version string
// (the minimum functional evidence).
func (c ExecCandidate) Validated() bool { return c.Version != "" }

// Managed reports whether the candidate is FreeIran-owned.
func (c ExecCandidate) Managed() bool { return c.Ownership == OwnershipManaged }

// EngineSpec describes one engine for discovery: its identity, the
// executable names it answers to and the bounded set of known
// installation subdirectories probed inside the platform roots.
type EngineSpec struct {
	// Name is the engine identity ("xray", "v2ray", "sing-box",
	// "tor", "psiphon").
	Name string

	// BinaryNames are the candidate executable file names. On Windows
	// each name is additionally probed with an ".exe" suffix.
	BinaryNames []string

	// Subdirs are known installation subdirectories probed inside the
	// platform search roots (e.g. "Xray" under %ProgramFiles%).
	Subdirs []string
}

// DefaultEngineSpecs returns the discovery specifications for every
// engine FreeIran supports: the protocol cores and the first-class
// providers.
func DefaultEngineSpecs() map[string]EngineSpec {
	return map[string]EngineSpec{
		"xray": {
			Name:        "xray",
			BinaryNames: []string{"xray"},
			Subdirs:     []string{"xray", "Xray", "xray-core", "Xray-core", "XTLS"},
		},
		"v2ray": {
			Name:        "v2ray",
			BinaryNames: []string{"v2ray"},
			Subdirs:     []string{"v2ray", "V2Ray", "v2fly", "V2Fly"},
		},
		"sing-box": {
			Name:        "sing-box",
			BinaryNames: []string{"sing-box"},
			Subdirs:     []string{"sing-box", "SagerNet", "SagerNet/sing-box"},
		},
		"tor": {
			Name:        "tor",
			BinaryNames: []string{"tor"},
			Subdirs: []string{
				"tor", "Tor",
				// Tor Browser's fixed inner layout
				// (Browser/TorBrowser/Tor/tor.exe).
				"Tor Browser/Browser/TorBrowser/Tor",
			},
		},
		"psiphon": {
			Name:        "psiphon",
			BinaryNames: []string{"consoleclient", "psiphon-tunnel-core"},
			Subdirs:     []string{"psiphon", "Psiphon", "Psiphon3", "PsiphonLabs"},
		},
	}
}

// probeEntry is the cached result of one version probe.
type probeEntry struct {
	identity FileIdentity
	version  string
	origin   Origin
	// probedAt is when the probe ran; freshness is bounded by the TTL.
	probedAt time.Time
	// negative marks a probe that produced no version output; negative
	// entries expire faster so a broken/unsupported binary is retried
	// sooner than a healthy one is re-probed.
	negative bool
}

// ExecDiscovery implements the bounded, cached, deduplicated
// executable discovery. Create one instance per application and share
// it everywhere (one discovery authority).
type ExecDiscovery struct {
	// mu guards specs, managedDirs and the cache.
	mu          sync.RWMutex
	specs       map[string]EngineSpec
	managedDirs map[string][]string
	cache       map[string]*probeEntry

	// flights deduplicates concurrent probes for the same
	// (engine, path, identity) key.
	flights singleflight.Group[string]

	// lookPath is injectable for tests (defaults to exec.LookPath).
	lookPath func(string) (string, error)

	// probe spawns one version probe. Injectable for tests; defaults
	// to queryCoreVersion (concealed console, 5s per probe form).
	probe func(ctx context.Context, path string) string

	// posTTL is how long a positive (validated) probe result is reused
	// before the next discovery re-probes; negTTL bounds negative
	// results. Both are bounded freshness policies — nothing is kept
	// forever.
	posTTL time.Duration
	negTTL time.Duration

	// now is injectable for tests.
	now func() time.Time
}

// DefaultProbeTTL is the default freshness window for a positive probe
// result. Within the window AND an unchanged file identity, discovery
// reuses the cached version without spawning a process.
const DefaultProbeTTL = 15 * time.Minute

// DefaultNegativeTTL is the shorter freshness window for probes that
// produced no version (broken or silent binary): retried sooner, but
// never on every call within the same burst.
const DefaultNegativeTTL = time.Minute

// NewExecDiscovery creates the discovery authority. The probe may be
// nil (defaults to the platform version probe); specs may be nil
// (defaults to DefaultEngineSpecs).
func NewExecDiscovery(probe func(ctx context.Context, path string) string, specs map[string]EngineSpec) *ExecDiscovery {
	if probe == nil {
		probe = queryCoreVersion
	}

	if specs == nil {
		specs = DefaultEngineSpecs()
	}

	return &ExecDiscovery{
		specs:       specs,
		managedDirs: make(map[string][]string),
		cache:       make(map[string]*probeEntry),
		lookPath:    execLookPath,
		probe:       probe,
		posTTL:      DefaultProbeTTL,
		negTTL:      DefaultNegativeTTL,
		now:         time.Now,
	}
}

// RegisterEngine adds or replaces an engine specification.
func (d *ExecDiscovery) RegisterEngine(spec EngineSpec) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.specs[spec.Name] = spec
}

// AddManagedDir registers a FreeIran-managed directory as the
// highest-priority search root for the engine (e.g.
// <workspace>/cores/xray/bin).
func (d *ExecDiscovery) AddManagedDir(engine, dir string) {
	if dir == "" {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, existing := range d.managedDirs[engine] {
		if existing == dir {
			return
		}
	}

	d.managedDirs[engine] = append(d.managedDirs[engine], dir)
}

// SetFreshness overrides the bounded freshness windows (tests).
func (d *ExecDiscovery) SetFreshness(pos, neg time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.posTTL, d.negTTL = pos, neg
}

// InvalidateEngine drops every cached probe result for one engine —
// used when an installation/update completes or the engine's runtime
// fails.
func (d *ExecDiscovery) InvalidateEngine(engine string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for key := range d.cache {
		if strings.HasPrefix(key, engine+"\x00") {
			delete(d.cache, key)
		}
	}
}

// InvalidatePath drops the cached probe result for one executable —
// used when a runtime failure invalidates the candidate.
func (d *ExecDiscovery) InvalidatePath(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for key, entry := range d.cache {
		if entry.identity.Path == path {
			delete(d.cache, key)
		}
	}
}

// InvalidateAll drops the entire probe cache — used when a
// configuration change affecting executable selection occurs or on an
// explicit Verify-all. It does NOT invalidate file identities, only
// probe reuse.
func (d *ExecDiscovery) InvalidateAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cache = make(map[string]*probeEntry)
}

// Discover resolves the best candidate for an engine: managed dirs
// first (priority 1), then PATH (priority 2), then the known platform
// installation locations (priority 3). The result carries origin,
// ownership and a validated (probed) version when available.
func (d *ExecDiscovery) Discover(ctx context.Context, engine string) (ExecCandidate, error) {
	candidates := d.DiscoverCandidates(ctx, engine)
	if len(candidates) == 0 {
		return ExecCandidate{}, firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "discover", "executable for engine %q not found in any controlled location", engine)
	}

	return candidates[0], nil
}

// DiscoverCandidates resolves every distinct installation of an engine
// it can find inside the controlled search roots, best candidate
// first: managed → PATH → system locations; validated before
// unvalidated within the same origin tier. Duplicate paths are
// deduplicated. Missing engines return an empty slice (never an
// error) so callers can treat "no candidates" as a normal state.
func (d *ExecDiscovery) DiscoverCandidates(ctx context.Context, engine string) []ExecCandidate {
	if d == nil {
		return nil
	}

	d.mu.RLock()
	spec, ok := d.specs[engine]
	managed := append([]string(nil), d.managedDirs[engine]...)
	d.mu.RUnlock()

	if !ok {
		return nil
	}

	// tier 1: managed dirs; tier 2: PATH; tier 3: platform roots.
	type located struct {
		path   string
		origin Origin
	}

	var found []located
	seen := make(map[string]bool)

	add := func(path string, origin Origin) {
		if path == "" || seen[path] {
			return
		}

		seen[path] = true
		found = append(found, located{path: path, origin: origin})
	}

	// Priority 1 — FreeIran managed locations.
	for _, dir := range managed {
		for _, name := range spec.binaryNames() {
			add(filepath.Join(dir, name), OriginManaged)
		}
	}

	// Priority 2 — the real OS PATH lookup. PATH remains authoritative
	// for normal system resolution.
	for _, base := range spec.BinaryNames {
		if path, err := d.lookPath(base); err == nil && path != "" {
			add(path, OriginPath)
		}
	}

	// Priority 3 — known platform installation locations (bounded).
	for _, path := range platformInstallPaths(spec) {
		add(path, OriginSystem)
	}

	// Identify + validate: probe each distinct path (cached,
	// deduplicated in flight), keeping file identity with the result.
	now := d.now().UTC()

	candidates := make([]ExecCandidate, 0, len(found))

	for _, loc := range found {
		cand, ok := d.identify(ctx, engine, loc.path, loc.origin, now)
		if !ok {
			continue
		}

		candidates = append(candidates, cand)
	}

	// Deterministic ordering: managed before external; within a tier,
	// validated (probed) before unvalidated; then stable path order.
	// The candidate set is small (a handful of installations at most),
	// so a full sort is cheap and keeps the result explainable.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]

		if a.Ownership != b.Ownership {
			return a.Ownership == OwnershipManaged
		}

		if a.Origin != b.Origin {
			return originRank(a.Origin) < originRank(b.Origin)
		}

		if a.Validated() != b.Validated() {
			return a.Validated()
		}

		return a.Path < b.Path
	})

	return candidates
}

// identify stats the path and returns the candidate with its (cached)
// probe result. Files that vanished between listing and probing are
// skipped; stale cache entries whose file identity changed are
// transparently re-probed.
func (d *ExecDiscovery) identify(ctx context.Context, engine, path string, origin Origin, now time.Time) (ExecCandidate, bool) {
	identity, ok := fileIdentityOf(path)
	if !ok {
		// Path disappeared since listing — never serve a stale entry
		// for a vanished file.
		d.dropCache(path)

		return ExecCandidate{}, false
	}

	key := engine + "\x00" + path

	d.mu.RLock()
	entry, hasEntry := d.cache[key]
	d.mu.RUnlock()

	if hasEntry && d.fresh(entry, now) && entry.identity.Size == identity.Size && entry.identity.ModTime.Equal(identity.ModTime) {
		// Cache hit: same identity, within the freshness window. No
		// process spawn.
		return ExecCandidate{
			Engine:    engine,
			Path:      path,
			Origin:    origin,
			Ownership: ownershipOf(origin),
			Version:   entry.version,
			Size:      identity.Size,
			ModTime:   identity.ModTime,
			LastSeen:  now,
		}, true
	}

	// Cache miss, stale TTL or changed identity: probe (deduplicated).
	version := d.probeCached(ctx, key, identity, origin)

	return ExecCandidate{
		Engine:    engine,
		Path:      path,
		Origin:    origin,
		Ownership: ownershipOf(origin),
		Version:   version,
		Size:      identity.Size,
		ModTime:   identity.ModTime,
		LastSeen:  now,
	}, true
}

// probeCached runs (or reuses) the version probe for one identity,
// deduplicating concurrent probes for the same key: the first caller
// spawns the process, every concurrent caller shares the result.
func (d *ExecDiscovery) probeCached(ctx context.Context, key string, identity FileIdentity, origin Origin) string {
	version, _ := singleflight.Do(d.flightsOf(), ctx, key,
		// The shared probe is detached from the first caller's context:
		// one cancelled caller must not cancel the shared probe. It is
		// bounded instead by its own timeout — queryCoreVersion applies
		// 5s per probe form, so an overall 30s cap bounds the worst case.
		func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		},
		func(execCtx context.Context) (string, error) {
			// Double-check under the flight: another execution may have
			// populated a fresh entry for this identity between our
			// caller's cache miss and this execution starting. Without
			// this check, a caller that raced a JUST-completed probe
			// would spawn a duplicate process — the exact duplicate
			// work v0.9.14 forbids.
			d.mu.RLock()

			if entry, ok := d.cache[key]; ok &&
				entry.identity.Size == identity.Size &&
				entry.identity.ModTime.Equal(identity.ModTime) &&
				d.fresh(entry, d.now().UTC()) {
				d.mu.RUnlock()

				return entry.version, nil
			}

			d.mu.RUnlock()

			v := d.probe(execCtx, identity.Path)

			d.mu.Lock()
			d.cache[key] = &probeEntry{
				identity: identity,
				version:  v,
				origin:   origin,
				probedAt: d.now().UTC(),
				negative: v == "",
			}
			d.mu.Unlock()

			return v, nil
		})

	return version
}

// flights lazily allocates the singleflight group (kept as a value so
// the zero ExecDiscovery stays unusable but explicit).
func (d *ExecDiscovery) flightsOf() *singleflight.Group[string] { return &d.flights }

// fresh reports whether an entry is still within its bounded
// freshness window.
func (d *ExecDiscovery) fresh(entry *probeEntry, now time.Time) bool {
	ttl := d.posTTL
	if entry.negative {
		ttl = d.negTTL
	}

	return now.Sub(entry.probedAt) < ttl
}

func (d *ExecDiscovery) dropCache(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for key, entry := range d.cache {
		if entry.identity.Path == path {
			delete(d.cache, key)
		}
	}
}

// binaryNames returns the engine's candidate file names, with the
// platform executable suffix applied on Windows.
func (s EngineSpec) binaryNames() []string {
	names := make([]string, 0, len(s.BinaryNames)*2)

	for _, base := range s.BinaryNames {
		names = append(names, executableName(base))
	}

	return names
}

// ownershipOf maps an origin to its ownership class. Managed
// locations are FreeIran-owned; everything else is external and only
// ever referenced.
func ownershipOf(origin Origin) Ownership {
	if origin == OriginManaged {
		return OwnershipManaged
	}

	return OwnershipExternal
}

// originRank orders the origin tiers (lower wins).
func originRank(o Origin) int {
	switch o {
	case OriginManaged:
		return 0
	case OriginPath:
		return 1
	case OriginSystem:
		return 2
	case OriginUser:
		return 3
	default:
		return 4
	}
}
