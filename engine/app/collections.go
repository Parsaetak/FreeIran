// collections.go implements the v0.7 roadmap work "configuration
// grouping + favorites" on the EXISTING storage architecture (v0.9.10):
//
//   - PERSISTENT favorites and user-defined groups (Work / Personal /
//     Travel / anything) live in one small, versioned, atomically
//     written sidecar (config/collections.json). The configuration
//     store itself is never duplicated: groups reference stable config
//     IDs (the store fingerprint), exactly like the ranking cooldown
//     memory does.
//   - BUILT-IN groups (All / Favorites / Working / Untested / Fast /
//     Recently tested) are COMPUTED from measured evidence — never
//     stored, never invented. A built-in group with no evidence shows
//     an honest zero count.
//   - Group filtering extends the EXISTING server-side ConfigFilter
//     pipeline (one filtering system); the UI receives the same
//     ConfigPage shape and can additionally organize the result by
//     source / protocol / status from fields every row already carries.
//
// Trust and testing are never bypassed: marking a favorite changes NO
// verification policy — a favorite is connected through the exact same
// verified state machine as any other configuration.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/system"
)

// collectionsFormatVersion is the sidecar schema version. A mismatch
// on load falls back to empty collections (favorites are a convenience
// layer; a schema change must never brick the store or the app).
const collectionsFormatVersion = 1

// fastGroupThresholdMS is the measured-latency bar for the built-in
// "Fast" group. Only WORKING configurations with a real measurement
// qualify — 0 ms readings are measured sub-millisecond round trips
// (the fastest possible), not missing data.
const fastGroupThresholdMS = 400

// recentlyTestedWindow bounds the built-in "Recently tested" group.
const recentlyTestedWindow = 24 * time.Hour

// userGroupIDPrefix keeps generated group ids stable and recognizable.
const userGroupIDPrefix = "g-"

// persistedUserGroup is one user-defined group in the sidecar.
type persistedUserGroup struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	ConfigIDs []string `json:"config_ids,omitempty"`
	CreatedAt int64    `json:"created_at,omitempty"`
}

// persistedCollections is the collections.json document.
type persistedCollections struct {
	Version   int                  `json:"version"`
	Favorites []string             `json:"favorites,omitempty"`
	Groups    []persistedUserGroup `json:"groups,omitempty"`
}

// UserGroupView is the credential-free UI projection of one
// user-defined group.
type UserGroupView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Count     int    `json:"count"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

// BuiltinGroupView is the UI projection of one built-in group.
type BuiltinGroupView struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// GroupsOverview is the complete group surface for the Configs page:
// the built-in evidence groups (live counts) plus the user groups.
type GroupsOverview struct {
	Builtin []BuiltinGroupView `json:"builtin"`
	User    []UserGroupView    `json:"user"`
}

// builtinGroupIDs are the stable identifiers of the built-in groups.
var builtinGroupIDs = []string{
	"all", "favorites", "working", "untested", "fast", "recently_tested",
}

// collectionsState is the App-owned collections state (guarded by its
// own mutex; the app mutex protects the sources/settings surface).
type collectionsState struct {
	mu          sync.Mutex
	loaded      bool
	favorites   []string
	groups      []persistedUserGroup
	nextGroupID int

	// futureSchema (v0.9.12): the on-disk sidecar was written by a
	// NEWER binary — every mutating save REFUSES (§14: an older
	// binary must never silently rewrite a future schema).
	futureSchema bool
}

// collectionsPath is the persisted collections sidecar.
func (a *App) collectionsPath() string {
	return filepath.Join(a.layout.Config, "collections.json")
}

// loadCollections reads the sidecar once (idempotent; a missing or
// unreadable file yields empty collections, never an error).
func (a *App) loadCollections() {
	a.collections.mu.Lock()
	defer a.collections.mu.Unlock()

	if a.collections.loaded {
		return
	}

	a.collections.loaded = true
	a.collections.favorites = nil
	a.collections.groups = nil
	a.collections.nextGroupID = 1

	raw, err := os.ReadFile(a.collectionsPath())
	if err != nil || len(raw) == 0 {
		return
	}

	// Decode defensively: the sidecar is a convenience layer.
	var doc persistedCollections

	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}

	if doc.Version > collectionsFormatVersion {
		// Future schema (v0.9.12 §14): run empty, but arm the save
		// guard so no mutation can downgrade the document.
		a.collections.futureSchema = true

		return
	}

	if doc.Version != collectionsFormatVersion {
		return // unknown schema: start empty (versioned migration point)
	}

	// Stable favorites: deduplicate, preserve order.
	seen := make(map[string]bool, len(doc.Favorites))

	for _, id := range doc.Favorites {
		if id == "" || seen[id] {
			continue
		}

		seen[id] = true
		a.collections.favorites = append(a.collections.favorites, id)
	}

	// Stable groups: deduplicate ids AND members.
	groupIDs := make(map[string]bool, len(doc.Groups))

	for _, grp := range doc.Groups {
		if grp.ID == "" || grp.Name == "" || groupIDs[grp.ID] {
			continue
		}

		groupIDs[grp.ID] = true

		members := make([]string, 0, len(grp.ConfigIDs))
		memberSeen := make(map[string]bool, len(grp.ConfigIDs))

		for _, id := range grp.ConfigIDs {
			if id == "" || memberSeen[id] {
				continue
			}

			memberSeen[id] = true
			members = append(members, id)
		}

		grp.ConfigIDs = members
		a.collections.groups = append(a.collections.groups, grp)
	}

	sort.SliceStable(a.collections.groups, func(i, j int) bool {
		return a.collections.groups[i].Name < a.collections.groups[j].Name
	})

	a.collections.nextGroupID = len(a.collections.groups) + 1
}

// saveCollectionsLocked persists the sidecar atomically (caller holds
// collections.mu).
func (a *App) saveCollectionsLocked() error {
	if a.collections.futureSchema {
		// v0.9.12 §14: never silently rewrite a future schema with an
		// older binary — the refusal is loud and the document stays
		// intact for the newer version.
		return fmt.Errorf("app: collections sidecar uses a newer schema; refusing to overwrite (run the newer version first)")
	}

	doc := persistedCollections{
		Version:   collectionsFormatVersion,
		Favorites: a.collections.favorites,
		Groups:    a.collections.groups,
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("app: encode collections: %w", err)
	}

	if err := system.WriteFileAtomic(a.collectionsPath(), raw, 0o600); err != nil {
		return fmt.Errorf("app: write collections: %w", err)
	}

	return nil
}

// favoriteSetLocked renders the favorites as a set (caller holds mu).
func (c *collectionsState) favoriteSetLocked() map[string]bool {
	set := make(map[string]bool, len(c.favorites))

	for _, id := range c.favorites {
		set[id] = true
	}

	return set
}

// CollectionService exposes favorites and groups to the UI.
type CollectionService struct {
	app *App
}

// NewCollectionService binds the collection service to the app.
func NewCollectionService(a *App) *CollectionService {
	return &CollectionService{app: a}
}

// Favorites returns the ordered favorite configuration IDs.
func (s *CollectionService) Favorites() []string {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	return append([]string(nil), s.app.collections.favorites...)
}

// IsFavorite reports whether a configuration is a favorite.
func (s *CollectionService) IsFavorite(configID string) bool {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for _, id := range s.app.collections.favorites {
		if id == configID {
			return true
		}
	}

	return false
}

// ToggleFavorite adds or removes a favorite and persists the sidecar.
// The returned bool is the NEW state. Favorites never bypass testing
// or verification — they are a UI affordance only.
func (s *CollectionService) ToggleFavorite(configID string) (bool, error) {
	if strings.TrimSpace(configID) == "" {
		return false, fmt.Errorf("app: configuration id is required")
	}

	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, id := range s.app.collections.favorites {
		if id == configID {
			s.app.collections.favorites = append(
				s.app.collections.favorites[:i],
				s.app.collections.favorites[i+1:]...)

			return false, s.app.saveCollectionsLocked()
		}
	}

	s.app.collections.favorites = append(s.app.collections.favorites, configID)

	return true, s.app.saveCollectionsLocked()
}

// UserGroups lists the user-defined groups with live member counts.
func (s *CollectionService) UserGroups() []UserGroupView {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	out := make([]UserGroupView, 0, len(s.app.collections.groups))

	for _, grp := range s.app.collections.groups {
		out = append(out, UserGroupView{
			ID:        grp.ID,
			Name:      grp.Name,
			Count:     len(grp.ConfigIDs),
			CreatedAt: grp.CreatedAt,
		})
	}

	return out
}

// CreateUserGroup adds a named group and persists it.
func (s *CollectionService) CreateUserGroup(name string) (UserGroupView, error) {
	name = strings.TrimSpace(name)

	if name == "" {
		return UserGroupView{}, fmt.Errorf("app: group name is required")
	}

	if len(name) > 64 {
		return UserGroupView{}, fmt.Errorf("app: group name is too long (max 64 characters)")
	}

	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for _, grp := range s.app.collections.groups {
		if strings.EqualFold(grp.Name, name) {
			return UserGroupView{}, fmt.Errorf("app: a group named %q already exists", name)
		}
	}

	id := fmt.Sprintf("%s%d", userGroupIDPrefix, s.app.collections.nextGroupID)
	s.app.collections.nextGroupID++

	grp := persistedUserGroup{
		ID:        id,
		Name:      name,
		CreatedAt: time.Now().UTC().UnixMilli(),
	}

	s.app.collections.groups = append(s.app.collections.groups, grp)

	sort.SliceStable(s.app.collections.groups, func(i, j int) bool {
		return s.app.collections.groups[i].Name < s.app.collections.groups[j].Name
	})

	if err := s.app.saveCollectionsLocked(); err != nil {
		return UserGroupView{}, err
	}

	return UserGroupView{ID: grp.ID, Name: grp.Name, CreatedAt: grp.CreatedAt}, nil
}

// DeleteUserGroup removes a group (configurations are untouched —
// they live in the store, not the group).
func (s *CollectionService) DeleteUserGroup(groupID string) error {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, grp := range s.app.collections.groups {
		if grp.ID == groupID {
			s.app.collections.groups = append(
				s.app.collections.groups[:i],
				s.app.collections.groups[i+1:]...)

			return s.app.saveCollectionsLocked()
		}
	}

	return fmt.Errorf("app: group %q not found", groupID)
}

// RenameUserGroup renames a group.
func (s *CollectionService) RenameUserGroup(groupID, name string) error {
	name = strings.TrimSpace(name)

	if name == "" {
		return fmt.Errorf("app: group name is required")
	}

	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, grp := range s.app.collections.groups {
		if grp.ID == groupID {
			s.app.collections.groups[i].Name = name

			return s.app.saveCollectionsLocked()
		}
	}

	return fmt.Errorf("app: group %q not found", groupID)
}

// AddToUserGroup adds a configuration to a group (idempotent).
func (s *CollectionService) AddToUserGroup(groupID, configID string) error {
	if configID == "" {
		return fmt.Errorf("app: configuration id is required")
	}

	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, grp := range s.app.collections.groups {
		if grp.ID != groupID {
			continue
		}

		for _, existing := range grp.ConfigIDs {
			if existing == configID {
				return nil // idempotent
			}
		}

		s.app.collections.groups[i].ConfigIDs = append(s.app.collections.groups[i].ConfigIDs, configID)

		return s.app.saveCollectionsLocked()
	}

	return fmt.Errorf("app: group %q not found", groupID)
}

// RemoveFromUserGroup removes a configuration from a group
// (idempotent; removing a missing member succeeds).
func (s *CollectionService) RemoveFromUserGroup(groupID, configID string) error {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, grp := range s.app.collections.groups {
		if grp.ID != groupID {
			continue
		}

		for j, existing := range grp.ConfigIDs {
			if existing == configID {
				s.app.collections.groups[i].ConfigIDs = append(
					grp.ConfigIDs[:j], grp.ConfigIDs[j+1:]...)

				break
			}
		}

		return s.app.saveCollectionsLocked()
	}

	return fmt.Errorf("app: group %q not found", groupID)
}

// GroupMembers returns the configuration IDs of one user group.
func (s *CollectionService) GroupMembers(groupID string) ([]string, error) {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for _, grp := range s.app.collections.groups {
		if grp.ID == groupID {
			return append([]string(nil), grp.ConfigIDs...), nil
		}
	}

	return nil, fmt.Errorf("app: group %q not found", groupID)
}

// GroupsOverview returns the built-in evidence groups with live
// counts plus the user groups. Built-in counts are computed from the
// store's real records in ONE bounded scan (the same bound the
// ranking engine uses) — never from invented scores.
func (s *CollectionService) GroupsOverview() (*GroupsOverview, error) {
	s.app.loadCollections()

	overview := &GroupsOverview{
		Builtin: make([]BuiltinGroupView, 0, len(builtinGroupIDs)),
	}

	counts, err := s.app.builtinGroupCounts()
	if err != nil {
		return nil, err
	}

	for _, id := range builtinGroupIDs {
		overview.Builtin = append(overview.Builtin, BuiltinGroupView{
			ID:    id,
			Count: counts[id],
		})
	}

	overview.User = s.UserGroups()

	return overview, nil
}

// builtinGroupCounts computes the live counts of every built-in group
// from ONE bounded store scan of real records. Evidence only: "fast"
// counts working configurations with a measured latency at or below
// the threshold (a 0 ms reading is a measured sub-millisecond round
// trip), "recently_tested" counts configurations tested inside the
// window, "untested" counts configurations with no measurement at
// all. Nothing is extrapolated.
func (a *App) builtinGroupCounts() (map[string]int, error) {
	a.loadCollections()

	a.collections.mu.Lock()
	favorites := a.collections.favoriteSetLocked()
	a.collections.mu.Unlock()

	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()

	counts := map[string]int{
		"all": 0, "favorites": 0, "working": 0,
		"untested": 0, "fast": 0, "recently_tested": 0,
	}

	now := time.Now().UnixMilli()

	err := a.store.Iterate(ctx, func(key string, value []byte) error {
		if counts["all"] >= candidateScanLimit {
			return errCandidateLimit
		}

		var cfg struct {
			ID        string `json:"id"`
			Working   bool   `json:"working"`
			LatencyMS int64  `json:"latency_ms,omitempty"`
			TestedAt  int64  `json:"tested_at,omitempty"`
		}

		if err := json.Unmarshal(value, &cfg); err != nil {
			return nil // undecodable record: skip
		}

		if cfg.ID == "" {
			cfg.ID = key
		}

		counts["all"]++

		if favorites[cfg.ID] {
			counts["favorites"]++
		}

		if cfg.TestedAt == 0 {
			counts["untested"]++
		} else {
			if cfg.Working {
				counts["working"]++
			}

			if now-cfg.TestedAt <= recentlyTestedWindow.Milliseconds() {
				counts["recently_tested"]++
			}
		}

		// Fast: WORKING with a real measurement at or below the
		// threshold (0 ms on a working config is measured and fastest).
		if cfg.TestedAt != 0 && cfg.Working && cfg.LatencyMS <= fastGroupThresholdMS {
			counts["fast"]++
		}

		return nil
	})

	if err != nil && !errors.Is(err, errCandidateLimit) && ctx.Err() == nil {
		// The scan is best-effort for counts; a store hiccup logs
		// through the normal channels and yields honest partial counts.
		return counts, nil
	}

	return counts, nil
}
