// profiles.go implements the v0.9.11 roadmap feature "Connection
// Profiles" (P2 §18) on the EXISTING settings + connection engine:
//
//   - A profile is a persistent, NAMED SET OF CONNECTION PREFERENCES
//     (Home / Work / Travel / Privacy) — never a second configuration
//     store and never a second networking flow. Profiles carry only
//     preferences the engine already supports: the Quick Connect
//     provider mode (Auto / Configurations / Tor / Psiphon), an
//     optional selected configuration ID, an optional preferred core,
//     preferred SOCKS/HTTP ports and the existing recovery preference.
//     No credentials or secrets are ever stored in a profile.
//
//   - PERSISTENCE: one small, versioned, atomically written sidecar
//     (config/profiles.json) with stable profile IDs — the exact
//     architecture the collections sidecar (favorites/groups) uses.
//     Profiles REFERENCE stable configuration IDs; deleting a profile
//     never deletes a configuration, and a configuration that
//     disappears has its profile references pruned safely (never the
//     other way around).
//
//   - ACTIVATION passes through the ONE settings path
//     (persistSettings → validateSettings → applySettings → the live
//     connection manager) and every connection goes through the
//     existing Quick Connect / ConnectionService flows. A profile can
//     NEVER bypass route trust, configuration validation, testing,
//     verification, recovery safeguards or provider installation
//     safety — it only changes which preferences those systems use.
package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/system"
)

// profilesFormatVersion is the sidecar schema version. A HIGHER
// version on disk is left untouched and the sidecar starts empty
// (the documented forward-migration point: a newer schema must never
// be silently rewritten or mis-decoded by an older binary). Lower/
// equal versions decode; v1 is the initial schema.
const profilesFormatVersion = 1

// profileIDPrefix keeps generated profile ids stable and recognizable.
const profileIDPrefix = "p-"

// profileNameLimit bounds profile names (mirrors user groups).
const profileNameLimit = 64

// profileLimit bounds how many profiles one sidecar may carry.
const profileLimit = 32

// persistedProfile is one profile in the sidecar. Field semantics:
// all preference fields are EXPLICIT profile values — activation
// applies them as-is (0 ports = automatic allocation, empty backend
// = no preference: both are existing, meaningful engine values).
// AutoRecovery is the one tri-state: nil means "leave the current
// setting", non-nil is the desired recovery state.
type persistedProfile struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Mode             string `json:"mode"`
	ConfigID         string `json:"config_id,omitempty"`
	PreferredBackend string `json:"preferred_backend,omitempty"`
	LocalSocksPort   int    `json:"local_socks_port,omitempty"`
	LocalHTTPPort    int    `json:"local_http_port,omitempty"`
	AutoRecovery     *bool  `json:"auto_recovery,omitempty"`
	CreatedAt        int64  `json:"created_at,omitempty"`
}

// persistedProfiles is the profiles.json document.
type persistedProfiles struct {
	Version          int                `json:"version"`
	Profiles         []persistedProfile `json:"profiles,omitempty"`
	ActiveProfileID  string             `json:"active_profile_id,omitempty"`
	DefaultProfileID string             `json:"default_profile_id,omitempty"`
	NextID           int                `json:"next_id,omitempty"`
}

// ProfileView is the credential-free UI projection of one profile.
// ConfigName is resolved live from the store at view time so a stale
// reference is visible instead of invented.
type ProfileView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Mode             string `json:"mode"`
	ConfigID         string `json:"config_id,omitempty"`
	ConfigName       string `json:"config_name,omitempty"`
	ConfigAvailable  bool   `json:"config_available"`
	PreferredBackend string `json:"preferred_backend,omitempty"`
	LocalSocksPort   int    `json:"local_socks_port,omitempty"`
	LocalHTTPPort    int    `json:"local_http_port,omitempty"`
	AutoRecovery     *bool  `json:"auto_recovery,omitempty"`
	Active           bool   `json:"active"`
	Default          bool   `json:"default"`
	CreatedAt        int64  `json:"created_at,omitempty"`
}

// ProfileSpec is the create/update payload (no credentials).
type ProfileSpec struct {
	Name             string `json:"name"`
	Mode             string `json:"mode"`
	ConfigID         string `json:"config_id,omitempty"`
	PreferredBackend string `json:"preferred_backend,omitempty"`
	LocalSocksPort   int    `json:"local_socks_port,omitempty"`
	LocalHTTPPort    int    `json:"local_http_port,omitempty"`
	AutoRecovery     *bool  `json:"auto_recovery,omitempty"`
}

// profilesState is the App-owned profiles state (own mutex; the app
// mutex protects the settings surface).
type profilesState struct {
	mu        sync.Mutex
	loaded    bool
	profiles  []persistedProfile
	activeID  string
	defaultID string
	nextID    int

	// futureSchema (v0.9.12): the on-disk sidecar was written by a
	// NEWER binary. The service runs with empty profiles and every
	// mutating operation REFUSES to save — silently rewriting a
	// future schema with an older binary is forbidden (§14).
	futureSchema bool
}

// profilesPath is the persisted profiles sidecar.
func (a *App) profilesPath() string {
	return filepath.Join(a.layout.Config, "profiles.json")
}

// loadProfiles reads the sidecar once (idempotent). A missing or
// unreadable file yields empty profiles, never an error; an unknown
// FUTURE schema version leaves the file untouched and starts empty
// (forward-migration point).
func (a *App) loadProfiles() {
	a.profiles.mu.Lock()
	defer a.profiles.mu.Unlock()

	if a.profiles.loaded {
		return
	}

	a.profiles.loaded = true

	raw, err := os.ReadFile(a.profilesPath())
	if err != nil || len(raw) == 0 {
		a.profiles.nextID = 1

		return
	}

	var doc persistedProfiles

	if err := json.Unmarshal(raw, &doc); err != nil {
		a.profiles.nextID = 1

		return
	}

	if doc.Version > profilesFormatVersion {
		// Future schema: keep the file on disk for migration by
		// the newer version; run with empty profiles meanwhile —
		// and arm the save guard so no mutation can downgrade
		// the document behind the user's back.
		a.profiles.futureSchema = true
		a.profiles.nextID = 1

		return
	}

	// Stable profiles: deduplicate ids, validate every field, keep
	// a deterministic order.
	seen := make(map[string]bool, len(doc.Profiles))

	for _, profile := range doc.Profiles {
		if profile.ID == "" || seen[profile.ID] {
			continue
		}

		if normalizeProfile(&profile) != nil {
			continue // undecodable/hostile record: skip
		}

		seen[profile.ID] = true
		a.profiles.profiles = append(a.profiles.profiles, profile)
	}

	sortProfilesLocked(a.profiles.profiles)

	a.profiles.activeID = a.matchingProfileIDLocked(doc.ActiveProfileID)
	a.profiles.defaultID = a.matchingProfileIDLocked(doc.DefaultProfileID)

	a.profiles.nextID = len(a.profiles.profiles) + 1

	a.advanceNextIDLocked()
}

// advanceNextIDLocked keeps nextID above every existing numeric
// suffix so generated ids never collide with loaded ones.
func (a *App) advanceNextIDLocked() {
	for _, profile := range a.profiles.profiles {
		var n int

		if _, err := fmt.Sscanf(profile.ID, profileIDPrefix+"%d", &n); err == nil && n >= a.profiles.nextID {
			a.profiles.nextID = n + 1
		}
	}
}

// matchingProfileIDLocked returns id when it names a stored profile
// (dangling active/default references are dropped on load).
func (a *App) matchingProfileIDLocked(id string) string {
	for _, profile := range a.profiles.profiles {
		if profile.ID == id {
			return id
		}
	}

	return ""
}

// normalizeProfile validates and normalizes one persisted profile.
// It returns an error for records that cannot be trusted (never
// panics on hostile sidecar data).
func normalizeProfile(profile *persistedProfile) error {
	profile.Name = strings.TrimSpace(profile.Name)

	if profile.Name == "" || len(profile.Name) > profileNameLimit {
		return fmt.Errorf("invalid profile name")
	}

	switch profile.Mode {
	case ProviderModeAuto, ProviderModeConfigs, ProviderModeTor, ProviderModePsiphon:
	default:
		return fmt.Errorf("invalid profile mode %q", profile.Mode)
	}

	profile.ConfigID = strings.TrimSpace(profile.ConfigID)
	profile.PreferredBackend = strings.TrimSpace(profile.PreferredBackend)

	switch profile.PreferredBackend {
	case "", "xray", "v2ray", "sing-box":
	default:
		return fmt.Errorf("invalid preferred backend %q", profile.PreferredBackend)
	}

	for _, port := range []int{profile.LocalSocksPort, profile.LocalHTTPPort} {
		if port != 0 && (port < 1024 || port > 65535) {
			return fmt.Errorf("invalid profile port %d", port)
		}
	}

	return nil
}

// validateProfileSpec validates a create/update payload.
func validateProfileSpec(spec ProfileSpec) (ProfileSpec, error) {
	spec.Name = strings.TrimSpace(spec.Name)

	if spec.Name == "" {
		return spec, fmt.Errorf("app: profile name is required")
	}

	if len(spec.Name) > profileNameLimit {
		return spec, fmt.Errorf("app: profile name is too long (max %d characters)", profileNameLimit)
	}

	switch spec.Mode {
	case ProviderModeAuto, ProviderModeConfigs, ProviderModeTor, ProviderModePsiphon:
	default:
		return spec, fmt.Errorf("app: unknown profile mode %q (want auto, configs, tor or psiphon)", spec.Mode)
	}

	spec.ConfigID = strings.TrimSpace(spec.ConfigID)

	switch spec.PreferredBackend {
	case "", "xray", "v2ray", "sing-box":
	default:
		return spec, fmt.Errorf("app: unknown preferred backend %q", spec.PreferredBackend)
	}

	for name, port := range map[string]int{
		"SOCKS": spec.LocalSocksPort,
		"HTTP":  spec.LocalHTTPPort,
	} {
		if port == 0 {
			continue
		}

		if port < 1024 || port > 65535 {
			return spec, fmt.Errorf("app: local %s port %d out of range (1024-65535, or 0 = automatic)", name, port)
		}
	}

	return spec, nil
}

// sortProfilesLocked orders profiles deterministically: name
// (case-insensitive), then id as the tiebreaker.
func sortProfilesLocked(profiles []persistedProfile) {
	sort.SliceStable(profiles, func(i, j int) bool {
		if low := strings.ToLower(profiles[i].Name); low != strings.ToLower(profiles[j].Name) {
			return low < strings.ToLower(profiles[j].Name)
		}

		return profiles[i].ID < profiles[j].ID
	})
}

// saveProfilesLocked persists the sidecar atomically (caller holds
// profiles.mu).
func (a *App) saveProfilesLocked() error {
	if a.profiles.futureSchema {
		// v0.9.12 §14: never silently rewrite a future schema with an
		// older binary. The refusal is LOUD — the mutation fails with
		// this error — and the on-disk document stays intact for the
		// newer version.
		return fmt.Errorf("app: profiles sidecar uses a newer schema; refusing to overwrite (run the newer version first)")
	}

	doc := persistedProfiles{
		Version:          profilesFormatVersion,
		Profiles:         a.profiles.profiles,
		ActiveProfileID:  a.profiles.activeID,
		DefaultProfileID: a.profiles.defaultID,
		NextID:           a.profiles.nextID,
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("app: encode profiles: %w", err)
	}

	if err := system.WriteFileAtomic(a.profilesPath(), raw, 0o600); err != nil {
		return fmt.Errorf("app: write profiles: %w", err)
	}

	return nil
}

// ProfileService exposes connection profiles to the UI.
type ProfileService struct {
	app *App
}

// NewProfileService binds the profile service to the app.
func NewProfileService(a *App) *ProfileService {
	return &ProfileService{app: a}
}

// List returns every profile in deterministic order with live
// Active/Default flags and the live configuration availability. A
// configuration that no longer resolves is reported honestly
// (ConfigAvailable=false) — never invented, never silently dropped
// here: persisted pruning happens at boot (initProfiles) and after
// configuration deletions (pruneMissingConfigReferences), while
// activation REFUSES a dangling reference loudly.
func (s *ProfileService) List() []ProfileView {
	s.app.loadProfiles()

	s.app.profiles.mu.Lock()
	defer s.app.profiles.mu.Unlock()

	out := make([]ProfileView, 0, len(s.app.profiles.profiles))

	for _, profile := range s.app.profiles.profiles {
		out = append(out, s.app.profileViewLocked(profile,
			profile.ID == s.app.profiles.activeID,
			profile.ID == s.app.profiles.defaultID))
	}

	return out
}

// profileViewLocked renders the UI projection (caller holds
// profiles.mu; the store read needs no app lock).
func (a *App) profileViewLocked(profile persistedProfile, active, def bool) ProfileView {
	view := ProfileView{
		ID:               profile.ID,
		Name:             profile.Name,
		Mode:             profile.Mode,
		ConfigID:         profile.ConfigID,
		PreferredBackend: profile.PreferredBackend,
		LocalSocksPort:   profile.LocalSocksPort,
		LocalHTTPPort:    profile.LocalHTTPPort,
		AutoRecovery:     profile.AutoRecovery,
		Active:           active,
		Default:          def,
		CreatedAt:        profile.CreatedAt,
	}

	if profile.ConfigID != "" {
		if raw, err := a.store.Get(profile.ConfigID); err == nil {
			var cfg struct {
				Name string `json:"name"`
			}

			if json.Unmarshal(raw, &cfg) == nil {
				view.ConfigName = cfg.Name
			}

			view.ConfigAvailable = true
		}
	}

	return view
}

// Create adds a profile and persists it. Validation happens through
// validateProfileSpec (same port/backend rules as the settings
// service); no credential field exists on the payload.
func (s *ProfileService) Create(spec ProfileSpec) (ProfileView, error) {
	spec, err := validateProfileSpec(spec)
	if err != nil {
		return ProfileView{}, err
	}

	s.app.loadProfiles()

	s.app.profiles.mu.Lock()
	defer s.app.profiles.mu.Unlock()

	if err := s.app.ensureUniqueNameLocked(spec.Name, ""); err != nil {
		return ProfileView{}, err
	}

	if len(s.app.profiles.profiles) >= profileLimit {
		return ProfileView{}, fmt.Errorf("app: too many profiles (max %d)", profileLimit)
	}

	id := fmt.Sprintf("%s%d", profileIDPrefix, s.app.profiles.nextID)
	s.app.profiles.nextID++

	profile := persistedProfile{
		ID:               id,
		Name:             spec.Name,
		Mode:             spec.Mode,
		ConfigID:         spec.ConfigID,
		PreferredBackend: spec.PreferredBackend,
		LocalSocksPort:   spec.LocalSocksPort,
		LocalHTTPPort:    spec.LocalHTTPPort,
		AutoRecovery:     spec.AutoRecovery,
		CreatedAt:        time.Now().UTC().UnixMilli(),
	}

	s.app.profiles.profiles = append(s.app.profiles.profiles, profile)
	sortProfilesLocked(s.app.profiles.profiles)

	if err := s.app.saveProfilesLocked(); err != nil {
		return ProfileView{}, err
	}

	return s.app.profileViewLocked(profile, false, false), nil
}

// ensureUniqueNameLocked rejects duplicate profile names
// (case-insensitive; a rename target is excluded via excludeID).
func (a *App) ensureUniqueNameLocked(name, excludeID string) error {
	for _, profile := range a.profiles.profiles {
		if profile.ID != excludeID && strings.EqualFold(profile.Name, name) {
			return fmt.Errorf("app: a profile named %q already exists", name)
		}
	}

	return nil
}

// Update replaces the editable fields of one profile.
func (s *ProfileService) Update(profileID string, spec ProfileSpec) (ProfileView, error) {
	spec, err := validateProfileSpec(spec)
	if err != nil {
		return ProfileView{}, err
	}

	s.app.loadProfiles()

	s.app.profiles.mu.Lock()
	defer s.app.profiles.mu.Unlock()

	index := s.app.indexOfProfileLocked(profileID)
	if index < 0 {
		return ProfileView{}, fmt.Errorf("app: profile %q not found", profileID)
	}

	if err := s.app.ensureUniqueNameLocked(spec.Name, profileID); err != nil {
		return ProfileView{}, err
	}

	profile := s.app.profiles.profiles[index]

	profile.Name = spec.Name
	profile.Mode = spec.Mode
	profile.ConfigID = spec.ConfigID
	profile.PreferredBackend = spec.PreferredBackend
	profile.LocalSocksPort = spec.LocalSocksPort
	profile.LocalHTTPPort = spec.LocalHTTPPort
	profile.AutoRecovery = spec.AutoRecovery

	s.app.profiles.profiles[index] = profile
	sortProfilesLocked(s.app.profiles.profiles)

	if err := s.app.saveProfilesLocked(); err != nil {
		return ProfileView{}, err
	}

	return s.app.profileViewLocked(profile,
		profile.ID == s.app.profiles.activeID,
		profile.ID == s.app.profiles.defaultID), nil
}

// Rename renames one profile (edit of the name field only).
func (s *ProfileService) Rename(profileID, name string) (ProfileView, error) {
	name = strings.TrimSpace(name)

	if name == "" {
		return ProfileView{}, fmt.Errorf("app: profile name is required")
	}

	if len(name) > profileNameLimit {
		return ProfileView{}, fmt.Errorf("app: profile name is too long (max %d characters)", profileNameLimit)
	}

	s.app.loadProfiles()

	s.app.profiles.mu.Lock()
	defer s.app.profiles.mu.Unlock()

	index := s.app.indexOfProfileLocked(profileID)
	if index < 0 {
		return ProfileView{}, fmt.Errorf("app: profile %q not found", profileID)
	}

	if err := s.app.ensureUniqueNameLocked(name, profileID); err != nil {
		return ProfileView{}, err
	}

	s.app.profiles.profiles[index].Name = name
	sortProfilesLocked(s.app.profiles.profiles)

	if err := s.app.saveProfilesLocked(); err != nil {
		return ProfileView{}, err
	}

	profile := s.app.profiles.profiles[index]

	return s.app.profileViewLocked(profile,
		profile.ID == s.app.profiles.activeID,
		profile.ID == s.app.profiles.defaultID), nil
}

// Duplicate copies one profile under a derived unique name. The copy
// is never active; it inherits every preference field.
func (s *ProfileService) Duplicate(profileID string) (ProfileView, error) {
	s.app.loadProfiles()

	s.app.profiles.mu.Lock()
	defer s.app.profiles.mu.Unlock()

	index := s.app.indexOfProfileLocked(profileID)
	if index < 0 {
		return ProfileView{}, fmt.Errorf("app: profile %q not found", profileID)
	}

	source := s.app.profiles.profiles[index]

	if len(s.app.profiles.profiles) >= profileLimit {
		return ProfileView{}, fmt.Errorf("app: too many profiles (max %d)", profileLimit)
	}

	name := s.app.derivedCopyNameLocked(source.Name)

	id := fmt.Sprintf("%s%d", profileIDPrefix, s.app.profiles.nextID)
	s.app.profiles.nextID++

	cloned := source
	cloned.ID = id
	cloned.Name = name
	cloned.CreatedAt = time.Now().UTC().UnixMilli()

	s.app.profiles.profiles = append(s.app.profiles.profiles, cloned)
	sortProfilesLocked(s.app.profiles.profiles)

	if err := s.app.saveProfilesLocked(); err != nil {
		return ProfileView{}, err
	}

	return s.app.profileViewLocked(cloned, false, false), nil
}

// derivedCopyNameLocked builds "X (copy)", "X (copy 2)", … unique
// among the stored profiles.
func (a *App) derivedCopyNameLocked(name string) string {
	candidate := name + " (copy)"

	for n := 2; ; n++ {
		taken := false

		for _, profile := range a.profiles.profiles {
			if strings.EqualFold(profile.Name, candidate) {
				taken = true

				break
			}
		}

		if !taken {
			return candidate
		}

		candidate = fmt.Sprintf("%s (copy %d)", name, n)
	}
}

// Delete removes one profile. Configurations are NEVER touched —
// they live in the store and a profile only references them. If the
// deleted profile was active or default, those markers are cleared
// (the runtime preferences themselves are left as they are: they are
// ordinary settings until another profile is applied).
func (s *ProfileService) Delete(profileID string) error {
	s.app.loadProfiles()

	s.app.profiles.mu.Lock()
	defer s.app.profiles.mu.Unlock()

	index := s.app.indexOfProfileLocked(profileID)
	if index < 0 {
		return fmt.Errorf("app: profile %q not found", profileID)
	}

	s.app.profiles.profiles = append(
		s.app.profiles.profiles[:index],
		s.app.profiles.profiles[index+1:]...)

	if s.app.profiles.activeID == profileID {
		s.app.profiles.activeID = ""
	}

	if s.app.profiles.defaultID == profileID {
		s.app.profiles.defaultID = ""
	}

	return s.app.saveProfilesLocked()
}

// SetDefault marks one profile as the startup profile: it is applied
// (in memory, through the one settings path) on every boot until the
// marker is cleared or changed.
func (s *ProfileService) SetDefault(profileID string) error {
	s.app.loadProfiles()

	s.app.profiles.mu.Lock()

	if s.app.indexOfProfileLocked(profileID) < 0 {
		s.app.profiles.mu.Unlock()

		return fmt.Errorf("app: profile %q not found", profileID)
	}

	s.app.profiles.defaultID = profileID

	if err := s.app.saveProfilesLocked(); err != nil {
		s.app.profiles.mu.Unlock()

		return err
	}

	s.app.profiles.mu.Unlock()

	s.app.applyDefaultProfileAtBoot()

	return nil
}

// ClearDefault removes the startup-profile marker.
func (s *ProfileService) ClearDefault() error {
	s.app.loadProfiles()

	s.app.profiles.mu.Lock()
	defer s.app.profiles.mu.Unlock()

	s.app.profiles.defaultID = ""

	return s.app.saveProfilesLocked()
}

// Active returns the currently active profile (ok=false when no
// profile is active).
func (s *ProfileService) Active() (ProfileView, bool, error) {
	s.app.loadProfiles()

	s.app.profiles.mu.Lock()
	defer s.app.profiles.mu.Unlock()

	for _, profile := range s.app.profiles.profiles {
		if profile.ID == s.app.profiles.activeID {
			return s.app.profileViewLocked(profile, true,
				profile.ID == s.app.profiles.defaultID), true, nil
		}
	}

	return ProfileView{}, false, nil
}

// SetActive selects the active profile and applies its preferences
// through the ONE settings path (persistSettings → validateSettings →
// applySettings → the live connection manager). The preferences take
// effect on the next connect — which runs through the existing
// Quick Connect / ConnectionService flows with their verification,
// trust and recovery rules UNCHANGED. A profile never connects by
// itself and can never bypass any safety boundary.
//
// v0.9.11 coherence contract: the settings mutation and the
// active-marker write happen under ONE profiles.mu critical section,
// so two concurrent activations can never tear the state (the marker
// of one profile with the preferences of another). The lock order
// profiles.mu → a.mu is the same one applyDefaultProfileAtBoot uses,
// and no path takes them in reverse.
func (s *ProfileService) SetActive(profileID string) (ProfileView, error) {
	s.app.loadProfiles()

	s.app.profiles.mu.Lock()
	defer s.app.profiles.mu.Unlock()

	index := s.app.indexOfProfileLocked(profileID)
	if index < 0 {
		return ProfileView{}, fmt.Errorf("app: profile %q not found", profileID)
	}

	profile := s.app.profiles.profiles[index]

	// A configs-mode profile referencing a missing configuration
	// must fail LOUDLY here instead of silently connecting
	// elsewhere.
	if profile.Mode == ProviderModeConfigs && profile.ConfigID != "" {
		if _, err := s.app.store.Get(profile.ConfigID); err != nil {
			return ProfileView{}, fmt.Errorf(
				"app: profile %q references configuration %s which no longer exists; "+
					"edit the profile and select another configuration",
				profile.Name, profile.ConfigID)
		}
	}

	// Apply through the ONE settings path (validates + persists +
	// routes into the live manager), INSIDE the profiles critical
	// section so activation is atomic against other activations
	// and against the boot-time default-profile application.
	if err := s.app.persistSettings(func(settings *Settings) {
		settings.ProviderMode = profile.Mode

		settings.PreferredBackend = profile.PreferredBackend

		settings.LocalSocksPort = profile.LocalSocksPort

		settings.LocalHTTPPort = profile.LocalHTTPPort

		if profile.AutoRecovery != nil {
			settings.DisableAutoRecovery = !*profile.AutoRecovery
		}
	}); err != nil {
		return ProfileView{}, err
	}

	s.app.profiles.activeID = profile.ID

	if err := s.app.saveProfilesLocked(); err != nil {
		return ProfileView{}, err
	}

	return s.app.profileViewLocked(profile, true,
		profile.ID == s.app.profiles.defaultID), nil
}

// indexOfProfileLocked returns the slice index of profileID
// (caller holds profiles.mu).
func (a *App) indexOfProfileLocked(profileID string) int {
	for i, profile := range a.profiles.profiles {
		if profile.ID == profileID {
			return i
		}
	}

	return -1
}

// pruneMissingConfigReferences clears profile references to
// configurations that no longer exist in the store (the
// "configuration deleted" cleanup contract: profiles adapt safely —
// the store is authoritative and never touched). Bounded by the
// profile count.
func (a *App) pruneMissingConfigReferences() {
	a.loadProfiles()

	a.profiles.mu.Lock()
	defer a.profiles.mu.Unlock()

	changed := false

	for i := range a.profiles.profiles {
		id := a.profiles.profiles[i].ConfigID

		if id == "" {
			continue
		}

		if _, err := a.store.Get(id); err != nil {
			a.profiles.profiles[i].ConfigID = ""
			changed = true
		}
	}

	if changed {
		if err := a.saveProfilesLocked(); err != nil {
			// Persistence hiccup: the in-memory prune still holds
			// for this run; the error is surfaced through logging
			// (a convenience layer must never break the app).
			if a.logger != nil {
				a.logger.Warn("app", "profiles_prune", "prune profiles: %v", err)
			}
		}
	}
}

// applyDefaultProfileAtBoot applies the DEFAULT profile's preferences
// in memory (never rewriting settings.json): the persisted default
// marker decides which named preference set is in effect at startup.
// Called from boot and after SetDefault.
//
// The whole read-modify-write runs under ONE profiles.mu critical
// section (the same order SetActive uses: profiles.mu → a.mu), so a
// concurrent activation can never interleave with the boot
// application and leave half-applied preferences behind.
func (a *App) applyDefaultProfileAtBoot() {
	a.loadProfiles()

	a.profiles.mu.Lock()
	defer a.profiles.mu.Unlock()

	var profile *persistedProfile

	for i := range a.profiles.profiles {
		if a.profiles.profiles[i].ID == a.profiles.defaultID {
			profile = &a.profiles.profiles[i]

			break
		}
	}

	if profile == nil {
		return
	}

	settings := a.currentSettings()

	settings.ProviderMode = profile.Mode
	settings.PreferredBackend = profile.PreferredBackend
	settings.LocalSocksPort = profile.LocalSocksPort
	settings.LocalHTTPPort = profile.LocalHTTPPort

	if profile.AutoRecovery != nil {
		settings.DisableAutoRecovery = !*profile.AutoRecovery
	}

	// Validate before applying (the same rules persistSettings
	// enforces); an invalid stored profile degrades to the
	// persisted settings instead of breaking the boot.
	if err := validateSettings(settings); err != nil {
		if a.logger != nil {
			a.logger.Warn("app", "profiles_boot",
				"default profile %q failed validation, using persisted settings: %v",
				profile.Name, err)
		}

		return
	}

	a.mu.Lock()
	a.settings = settings
	a.mu.Unlock()

	a.applySettings(settings)

	if a.logger != nil {
		a.logger.Info("app", "profiles_boot",
			"startup profile %q applied (mode %s)", profile.Name, profile.Mode)
	}
}

// Boot-time wiring: called from New after settings load (see app.go).
func (a *App) initProfiles() {
	a.loadProfiles()

	// The configuration store is authoritative: references to
	// configurations that no longer exist are pruned once per boot
	// (activation additionally refuses a dangling reference loudly).
	a.pruneMissingConfigReferences()

	a.applyDefaultProfileAtBoot()
}
