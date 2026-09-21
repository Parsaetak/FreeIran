package app

// configorderservice.go implements v0.9.8.3 user-controlled
// configuration ordering:
//
//   - the order is a list of STABLE configuration IDs (never indexes);
//   - the whole ordered collection is persisted atomically;
//   - ListConfigs / ListConfigsFiltered apply the stored order after
//     loading, so filtering/sorting in the UI can never corrupt it and
//     a data refresh can never overwrite a newer user reorder;
//   - unknown/stale IDs (removed configs) are ignored; configs without
//     a stored position keep their store order after the ordered ones.
//
// Persistence is a small JSON document under the workspace config
// directory — one authority, no duplicate path subsystem.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/system"
)

// configOrderMax bounds the persisted order list (the store itself is
// the authority for what exists; this only bounds a corrupt file).
const configOrderMax = 50000

// configOrder is the persisted document.
type configOrder struct {
	Version   int      `json:"version"`
	UpdatedAt int64    `json:"updated_at"`
	Order     []string `json:"order"`
}

// configOrderState guards the cached order.
type configOrderState struct {
	mu    sync.Mutex
	known bool
	order []string
}

// configOrderPath is the persisted order file location.
func (a *App) configOrderPath() string {
	return filepath.Join(a.layout.Config, "order.json")
}

// loadConfigOrder reads the persisted order (empty when absent or
// corrupt — ordering is cosmetic and must never brick listing).
func (a *App) loadConfigOrder() []string {
	a.cfgOrder.mu.Lock()
	defer a.cfgOrder.mu.Unlock()

	if a.cfgOrder.known {
		return append([]string(nil), a.cfgOrder.order...)
	}

	raw, err := os.ReadFile(a.configOrderPath())
	if err == nil {
		var doc configOrder

		if json.Unmarshal(raw, &doc) == nil && len(doc.Order) <= configOrderMax {
			a.cfgOrder.order = append([]string(nil), doc.Order...)
		}
	}

	a.cfgOrder.known = true

	return append([]string(nil), a.cfgOrder.order...)
}

// saveConfigOrder persists the complete ordered collection atomically
// (temp file + rename) and updates the cache.
func (a *App) saveConfigOrder(order []string) error {
	if len(order) > configOrderMax {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "config-order", "order list too large (%d)", len(order))
	}

	doc := configOrder{
		Version:   1,
		UpdatedAt: time.Now().UTC().UnixMilli(),
		Order:     append([]string(nil), order...),
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}

	// v0.9.12: the ONE shared atomic-write path (temp + fsync +
	// rename) replaces the hand-rolled fixed-`.tmp` rename — the
	// fixed name could interleave between concurrent moves and a
	// crash could leave a torn file behind.
	if err := system.WriteFileAtomic(a.configOrderPath(), data, 0o600); err != nil {
		return err
	}

	a.cfgOrder.mu.Lock()
	a.cfgOrder.order = append([]string(nil), order...)
	a.cfgOrder.known = true
	a.cfgOrder.mu.Unlock()

	a.logger.Info("app", "config_order_saved",
		"configuration order persisted (%d ids)", len(order))

	return nil
}

// invalidateConfigOrder drops the cached order (used by tests and by
// migration paths that replace the config directory).
func (a *App) invalidateConfigOrder() {
	a.cfgOrder.mu.Lock()
	a.cfgOrder.known = false
	a.cfgOrder.order = nil
	a.cfgOrder.mu.Unlock()
}

// GetConfigOrder returns the stored order (may be empty).
func (s *DataService) GetConfigOrder() []string {
	return s.app.loadConfigOrder()
}

// SetConfigOrder persists the complete ordered collection of stable
// configuration IDs. IDs missing from the current store are pruned on
// the next apply; IDs missing from the provided list are appended in
// store order (the list the caller sends is authoritative).
func (s *DataService) SetConfigOrder(ids []string) error {
	cleaned := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))

	for _, id := range ids {
		if id == "" {
			continue
		}

		if _, dup := seen[id]; dup {
			continue
		}

		seen[id] = struct{}{}
		cleaned = append(cleaned, id)
	}

	return s.app.saveConfigOrder(cleaned)
}

// MoveConfig moves one configuration (by stable ID) to the given
// zero-based index within the complete ordered collection. It works
// for first→last, last→first, middle moves and repeated moves, and it
// never touches the underlying configurations — only their order.
func (s *DataService) MoveConfig(id string, toIndex int) error {
	if id == "" {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "config-order", "configuration id is required")
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 10*time.Second)
	defer cancel()

	// Build the complete collection: stored order first, then any
	// configs the order does not know about yet (store order).
	existing := s.app.ListAllConfigIDs(ctx)

	order := s.app.loadConfigOrder()
	order = pruneOrder(order, existing)

	ordered := append([]string(nil), order...)
	inOrder := make(map[string]struct{}, len(order))
	for _, v := range order {
		inOrder[v] = struct{}{}
	}

	for _, id2 := range existing {
		if _, ok := inOrder[id2]; !ok {
			ordered = append(ordered, id2)
		}
	}

	from := -1
	for i, v := range ordered {
		if v == id {
			from = i

			break
		}
	}

	if from < 0 {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "config-order", "configuration %s not found", id)
	}

	if toIndex < 0 {
		toIndex = 0
	}

	if toIndex >= len(ordered) {
		toIndex = len(ordered) - 1
	}

	// Classic remove + insert: works for every direction (first→last,
	// last→first, middle moves, repeated moves).
	moved := append([]string(nil), ordered...)
	item := moved[from]
	moved = append(moved[:from], moved[from+1:]...)
	moved = append(moved[:toIndex], append([]string{id}, moved[toIndex:]...)...)
	_ = item

	return s.app.saveConfigOrder(moved)
}

// pruneOrder drops order entries that no longer exist in the store.
func pruneOrder(order []string, existing []string) []string {
	live := make(map[string]struct{}, len(existing))
	for _, id := range existing {
		live[id] = struct{}{}
	}

	out := make([]string, 0, len(order))

	for _, id := range order {
		if _, ok := live[id]; ok {
			out = append(out, id)
		}
	}

	return out
}

// applyConfigOrder reorders loaded configurations according to the
// stored order (stable IDs; unknown IDs keep relative order after the
// ordered ones). Filtering and sorting in the UI never corrupt the
// underlying order because this runs on view data only.
func (a *App) applyConfigOrder(configs []config.Config) []config.Config {
	order := a.loadConfigOrder()
	if len(order) == 0 {
		return configs
	}

	pos := make(map[string]int, len(order))
	for i, id := range order {
		if _, dup := pos[id]; !dup {
			pos[id] = i
		}
	}

	ordered := make([]config.Config, 0, len(configs))
	unordered := make([]config.Config, 0, len(configs))

	for _, cfg := range configs {
		if _, ok := pos[cfg.ID]; ok {
			ordered = append(ordered, cfg)
		} else {
			unordered = append(unordered, cfg)
		}
	}

	// Stable sort of the ordered subset by stored position.
	byPos := func(i, j int) bool {
		return pos[ordered[i].ID] < pos[ordered[j].ID]
	}

	sortConfigsStable(ordered, byPos)

	return append(ordered, unordered...)
}

// sortConfigsStable sorts in place with the provided less function.
func sortConfigsStable(configs []config.Config, less func(i, j int) bool) {
	sort.SliceStable(configs, less)
}

// ListAllConfigIDs returns every stored configuration ID (bounded by
// the store; used to build the complete ordered collection).
func (a *App) ListAllConfigIDs(ctx context.Context) []string {
	ids := make([]string, 0, 256)

	err := a.store.Iterate(ctx, func(key string, _ []byte) error {
		ids = append(ids, key)

		return nil
	})

	if err != nil {
		a.logger.Warn("app", "config_order_scan",
			"configuration id scan ended early: %v", err)
	}

	return ids
}
