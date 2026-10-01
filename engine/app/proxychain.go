// proxychain.go implements v0.12.2 Proxy Chains on the EXISTING
// collections authority (engine/app/collections.go — the SAME
// sidecar, the SAME atomic-write path, the SAME future-schema
// refusal; there is no second grouping store).
//
// A proxy chain is an ORDERED list of existing configuration IDs
// (index 0 = first hop, last = egress) compiled into ONE protocol
// core process at connect time (Xray sockopt.dialerProxy / sing-box
// detour). It is never:
//   - a remote source (no fetch/refresh semantics),
//   - a configuration in the store (no credentials are copied —
//     chain records carry IDs only),
//   - a second connection path (ConnectChain rides the existing
//     connection state machine, verification gate and supervisor).
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/tester"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// ProxyChainView is the credential-free UI projection of one chain
// (list surface).
type ProxyChainView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Hops      int    `json:"hops"`
	CreatedAt int64  `json:"created_at,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// ProxyChainHopView is the credential-free projection of one hop:
// table-grade endpoint facts (protocol/name/host/port/transport/
// security) plus the hop's REAL measured evidence from the store.
type ProxyChainHopView struct {
	Position    int    `json:"position"`
	ConfigID    string `json:"config_id"`
	Name        string `json:"name,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Address     string `json:"address,omitempty"`
	Port        int    `json:"port,omitempty"`
	Transport   string `json:"transport,omitempty"`
	Security    string `json:"security,omitempty"`
	Working     bool   `json:"working"`
	LatencyMS   int64  `json:"latency_ms,omitempty"`
	TestedAt    int64  `json:"tested_at,omitempty"`
	TestBackend string `json:"test_backend,omitempty"`
	// Available reports whether the configuration still resolves in
	// the store (false = the chain needs editing before connecting).
	Available bool `json:"available"`
}

// ProxyChainDetails is the full editor/connect projection of one
// chain: identity, ordered hops with evidence, and the compiled
// preview ("A → B → C").
type ProxyChainDetails struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	ConfigIDs []string            `json:"config_ids"`
	Hops      []ProxyChainHopView `json:"hops"`
	Preview   string              `json:"preview,omitempty"`
	CreatedAt int64               `json:"created_at,omitempty"`
	UpdatedAt int64               `json:"updated_at,omitempty"`
	// Usable reports whether every hop currently resolves in the
	// store (a chain with missing hops refuses to connect).
	Usable bool `json:"usable"`
}

// ChainCheckResult is the honest outcome of one "Check chain"
// operation: per-hop stored evidence PLUS a fresh end-to-end
// measurement through the compiled chain. Chain readiness alone is
// never success — the EndToEnd result is the authority.
type ChainCheckResult struct {
	ChainID    string              `json:"chain_id"`
	Hops       []ProxyChainHopView `json:"hops"`
	EndToEnd   ChainE2EView        `json:"end_to_end"`
	DurationMS int64               `json:"duration_ms"`
}

// ChainE2EView projects the end-to-end probe result.
type ChainE2EView struct {
	OK        bool   `json:"ok"`
	PingMS    int64  `json:"ping_ms,omitempty"`
	Measured  bool   `json:"measured"`
	Backend   string `json:"backend,omitempty"`
	Quality   string `json:"quality,omitempty"`
	LastError string `json:"last_error,omitempty"`
	At        int64  `json:"at"`
}

// ProxyChainService exposes proxy chains to the UI over the SAME
// collections authority used by favorites and user groups.
type ProxyChainService struct {
	app *App
}

// NewProxyChainService binds the proxy-chain service to the app.
func NewProxyChainService(a *App) *ProxyChainService {
	return &ProxyChainService{app: a}
}

// ListProxyChains returns every chain with hop counts.
func (s *ProxyChainService) ListProxyChains() []ProxyChainView {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	out := make([]ProxyChainView, 0, len(s.app.collections.groups))

	for _, grp := range s.app.collections.groups {
		if grp.Kind != GroupKindProxyChain {
			continue
		}

		out = append(out, ProxyChainView{
			ID:        grp.ID,
			Name:      grp.Name,
			Hops:      len(grp.ConfigIDs),
			CreatedAt: grp.CreatedAt,
			UpdatedAt: grp.UpdatedAt,
		})
	}

	return out
}

// CreateProxyChain builds a chain from an ordered list of existing
// configuration IDs and persists it atomically.
func (s *ProxyChainService) CreateProxyChain(name string, configIDs []string) (ProxyChainView, error) {
	name = strings.TrimSpace(name)

	if name == "" {
		return ProxyChainView{}, fmt.Errorf("app: chain name is required")
	}

	if len(name) > 64 {
		return ProxyChainView{}, fmt.Errorf("app: chain name is too long (max 64 characters)")
	}

	if err := s.validateHopList(configIDs); err != nil {
		return ProxyChainView{}, err
	}

	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for _, grp := range s.app.collections.groups {
		if grp.Kind == GroupKindProxyChain && strings.EqualFold(grp.Name, name) {
			return ProxyChainView{}, fmt.Errorf("app: a chain named %q already exists", name)
		}
	}

	id := fmt.Sprintf("%s%d", proxyChainIDPrefix, s.app.collections.nextChainID)
	s.app.collections.nextChainID++

	now := time.Now().UTC().UnixMilli()

	grp := persistedGroup{
		ID:        id,
		Kind:      GroupKindProxyChain,
		Name:      name,
		ConfigIDs: append([]string(nil), configIDs...),
		CreatedAt: now,
		UpdatedAt: now,
	}

	s.app.collections.groups = append(s.app.collections.groups, grp)

	sort.SliceStable(s.app.collections.groups, func(i, j int) bool {
		return s.app.collections.groups[i].Name < s.app.collections.groups[j].Name
	})

	if err := s.app.saveCollectionsLocked(); err != nil {
		return ProxyChainView{}, err
	}

	return ProxyChainView{ID: id, Name: name, Hops: len(configIDs), CreatedAt: now, UpdatedAt: now}, nil
}

// RenameProxyChain renames one chain.
func (s *ProxyChainService) RenameProxyChain(chainID, name string) error {
	name = strings.TrimSpace(name)

	if name == "" {
		return fmt.Errorf("app: chain name is required")
	}

	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, grp := range s.app.collections.groups {
		if grp.ID == chainID && grp.Kind == GroupKindProxyChain {
			s.app.collections.groups[i].Name = name
			s.app.collections.groups[i].UpdatedAt = time.Now().UTC().UnixMilli()

			return s.app.saveCollectionsLocked()
		}
	}

	return fmt.Errorf("app: proxy chain %q not found", chainID)
}

// DeleteProxyChain removes one chain (configurations are untouched —
// they live in the store, not the chain).
func (s *ProxyChainService) DeleteProxyChain(chainID string) error {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, grp := range s.app.collections.groups {
		if grp.ID == chainID && grp.Kind == GroupKindProxyChain {
			s.app.collections.groups = append(
				s.app.collections.groups[:i],
				s.app.collections.groups[i+1:]...)

			return s.app.saveCollectionsLocked()
		}
	}

	return fmt.Errorf("app: proxy chain %q not found", chainID)
}

// AddHop inserts a configuration into the chain at position
// (0-based; position < 0 or >= hops appends).
func (s *ProxyChainService) AddHop(chainID, configID string, position int) error {
	if configID == "" {
		return fmt.Errorf("app: configuration id is required")
	}

	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, grp := range s.app.collections.groups {
		if grp.ID != chainID || grp.Kind != GroupKindProxyChain {
			continue
		}

		if len(grp.ConfigIDs) >= proxyChainMaxHops {
			return fmt.Errorf("app: chain %q already has the maximum of %d hops", grp.Name, proxyChainMaxHops)
		}

		for _, existing := range grp.ConfigIDs {
			if existing == configID {
				return fmt.Errorf("app: configuration %s is already a hop of chain %q", configID, grp.Name)
			}
		}

		if err := s.validateHopList(append(append([]string(nil), grp.ConfigIDs...), configID)); err != nil {
			return err
		}

		if position < 0 || position > len(grp.ConfigIDs) {
			position = len(grp.ConfigIDs)
		}

		hops := make([]string, 0, len(grp.ConfigIDs)+1)
		hops = append(hops, grp.ConfigIDs[:position]...)
		hops = append(hops, configID)
		hops = append(hops, grp.ConfigIDs[position:]...)

		s.app.collections.groups[i].ConfigIDs = hops
		s.app.collections.groups[i].UpdatedAt = time.Now().UTC().UnixMilli()

		return s.app.saveCollectionsLocked()
	}

	return fmt.Errorf("app: proxy chain %q not found", chainID)
}

// RemoveHop removes one configuration from the chain (a chain falling
// below two hops refuses — one hop is a plain connection, not a
// chain).
func (s *ProxyChainService) RemoveHop(chainID, configID string) error {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, grp := range s.app.collections.groups {
		if grp.ID != chainID || grp.Kind != GroupKindProxyChain {
			continue
		}

		for j, existing := range grp.ConfigIDs {
			if existing != configID {
				continue
			}

			if len(grp.ConfigIDs)-1 < proxyChainMinHops {
				return fmt.Errorf("app: chain %q needs at least %d hops — delete the chain instead", grp.Name, proxyChainMinHops)
			}

			hops := append(append([]string(nil), grp.ConfigIDs[:j]...), grp.ConfigIDs[j+1:]...)

			s.app.collections.groups[i].ConfigIDs = hops
			s.app.collections.groups[i].UpdatedAt = time.Now().UTC().UnixMilli()

			return s.app.saveCollectionsLocked()
		}
	}

	return fmt.Errorf("app: configuration %s is not a hop of chain %q", configID, chainID)
}

// ReorderHop moves one hop to a new 0-based position (clamped).
func (s *ProxyChainService) ReorderHop(chainID, configID string, position int) error {
	s.app.loadCollections()

	s.app.collections.mu.Lock()
	defer s.app.collections.mu.Unlock()

	for i, grp := range s.app.collections.groups {
		if grp.ID != chainID || grp.Kind != GroupKindProxyChain {
			continue
		}

		idx := -1

		for j, existing := range grp.ConfigIDs {
			if existing == configID {
				idx = j

				break
			}
		}

		if idx < 0 {
			return fmt.Errorf("app: configuration %s is not a hop of chain %q", configID, grp.Name)
		}

		if position < 0 || position >= len(grp.ConfigIDs) {
			position = len(grp.ConfigIDs) - 1
		}

		hops := append([]string(nil), grp.ConfigIDs...)
		copy(hops[idx:], hops[idx+1:])
		hops[len(hops)-1] = ""
		hops = hops[:len(hops)-1]

		hops = append(hops, "")
		copy(hops[position+1:], hops[position:])
		hops[position] = configID

		s.app.collections.groups[i].ConfigIDs = hops
		s.app.collections.groups[i].UpdatedAt = time.Now().UTC().UnixMilli()

		return s.app.saveCollectionsLocked()
	}

	return fmt.Errorf("app: proxy chain %q not found", chainID)
}

// ProxyChainDetails renders the full editor/connect projection.
func (s *ProxyChainService) ProxyChainDetails(chainID string) (*ProxyChainDetails, error) {
	s.app.loadCollections()

	s.app.collections.mu.Lock()

	var grp *persistedGroup

	for i := range s.app.collections.groups {
		if s.app.collections.groups[i].ID == chainID && s.app.collections.groups[i].Kind == GroupKindProxyChain {
			grp = &s.app.collections.groups[i]

			break
		}
	}

	if grp == nil {
		s.app.collections.mu.Unlock()

		return nil, fmt.Errorf("app: proxy chain %q not found", chainID)
	}

	ids := append([]string(nil), grp.ConfigIDs...)
	s.app.collections.mu.Unlock()

	return s.renderDetails(chainID, ids)
}

// ValidateProxyChain validates a candidate hop list (editor preview
// before save). The list need not be persisted.
func (s *ProxyChainService) ValidateProxyChain(configIDs []string) error {
	return s.validateHopList(configIDs)
}

// CheckChain runs the honest chain check: every hop's REAL stored
// evidence plus a FRESH end-to-end measurement through the compiled
// chain (one core process, bounded, instance closed afterwards).
// Per-hop readiness alone is never success.
func (s *ProxyChainService) CheckChain(chainID string) (*ChainCheckResult, error) {
	details, err := s.ProxyChainDetails(chainID)
	if err != nil {
		return nil, err
	}

	if !details.Usable {
		return nil, fmt.Errorf("app: chain %q has missing hops — edit the chain before checking", details.Name)
	}

	result := &ChainCheckResult{
		ChainID: chainID,
		Hops:    details.Hops,
	}

	started := time.Now()

	synthetic, err := s.app.buildChainConfig(chainID)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 90*time.Second)
	defer cancel()

	res := s.app.tester.Test(ctx, synthetic)

	result.EndToEnd = ChainE2EView{
		OK:        res.Working,
		PingMS:    res.PingMS,
		Measured:  res.Measured,
		Backend:   res.Backend,
		Quality:   res.Quality,
		LastError: res.LastError,
		At:        res.TestedAt.UnixMilli(),
	}
	result.DurationMS = time.Since(started).Milliseconds()

	logging.LogR(loggingRecordChainCheck(chainID, details.Name, res))

	return result, nil
}

// --- internals ---------------------------------------------------------

// validateHopList enforces every chain invariant against the store:
// bounded hop count, no empty entries, no duplicates, no missing
// configurations, no nested chains (a hop id must be a STORE id, not
// a chain id), and no self-reference. Cycles are structurally
// impossible (hops are store records, chains are not) and the length
// bound makes that guarantee explicit.
func (s *ProxyChainService) validateHopList(configIDs []string) error {
	if len(configIDs) < proxyChainMinHops {
		return fmt.Errorf("app: a proxy chain needs at least %d configurations", proxyChainMinHops)
	}

	if len(configIDs) > proxyChainMaxHops {
		return fmt.Errorf("app: a proxy chain is limited to %d hops (got %d)", proxyChainMaxHops, len(configIDs))
	}

	seen := make(map[string]bool, len(configIDs))

	for i, id := range configIDs {
		if id == "" {
			return fmt.Errorf("app: hop %d is empty", i+1)
		}

		if strings.HasPrefix(id, proxyChainIDPrefix) {
			return fmt.Errorf("app: hop %d references a proxy chain — chains cannot be nested", i+1)
		}

		if seen[id] {
			return fmt.Errorf("app: hop %d duplicates an earlier hop (%s)", i+1, id)
		}

		if _, err := s.app.store.Get(id); err != nil {
			return fmt.Errorf("app: hop %d configuration not found (%s) — refresh sources or pick another configuration", i+1, id)
		}

		seen[id] = true
	}

	return nil
}

// renderDetails loads every hop and projects it (caller must NOT hold
// the collections lock — the store scans run outside it).
func (s *ProxyChainService) renderDetails(chainID string, ids []string) (*ProxyChainDetails, error) {
	details := &ProxyChainDetails{
		ID:        chainID,
		ConfigIDs: ids,
		Hops:      make([]ProxyChainHopView, 0, len(ids)),
		Usable:    true,
	}

	names := make([]string, 0, len(ids))

	s.app.collections.mu.Lock()

	for _, grp := range s.app.collections.groups {
		if grp.ID == chainID && grp.Kind == GroupKindProxyChain {
			details.Name = grp.Name
			details.CreatedAt = grp.CreatedAt
			details.UpdatedAt = grp.UpdatedAt

			break
		}
	}

	s.app.collections.mu.Unlock()

	if details.Name == "" {
		return nil, fmt.Errorf("app: proxy chain %q not found", chainID)
	}

	for i, id := range ids {
		hop := ProxyChainHopView{
			Position:  i,
			ConfigID:  id,
			Available: false,
		}

		cfg, err := s.app.decodeHop(id)
		if err != nil {
			details.Usable = false
			details.Hops = append(details.Hops, hop)
			names = append(names, "missing:"+id)

			continue
		}

		hop.Available = true
		hop.Name = cfg.Name
		hop.Protocol = string(cfg.Type)
		hop.Address = cfg.Address
		hop.Port = cfg.Port
		hop.Transport = cfg.Network
		hop.Security = cfg.Security
		hop.Working = cfg.Working
		hop.LatencyMS = cfg.LatencyMS
		hop.TestedAt = cfg.TestedAt
		hop.TestBackend = cfg.TestBackend

		details.Hops = append(details.Hops, hop)
		names = append(names, cfg.Name)
	}

	details.Preview = strings.Join(names, " → ")

	return details, nil
}

// buildChainConfig composes the synthetic EXIT configuration for a
// chain: a deep copy of the egress hop carrying deep copies of the
// earlier hops in cfg.Chain. The composition is in-memory only — the
// store never gains a chain record, and chain records never copy
// configuration payloads (only ids).
func (a *App) buildChainConfig(chainID string) (config.Config, error) {
	a.loadCollections()

	a.collections.mu.Lock()

	var grp *persistedGroup

	for i := range a.collections.groups {
		if a.collections.groups[i].ID == chainID && a.collections.groups[i].Kind == GroupKindProxyChain {
			grp = &a.collections.groups[i]

			break
		}
	}

	if grp == nil {
		a.collections.mu.Unlock()

		return config.Config{}, fmt.Errorf("app: proxy chain %q not found", chainID)
	}

	name := grp.Name
	ids := append([]string(nil), grp.ConfigIDs...)

	a.collections.mu.Unlock()

	if len(ids) < proxyChainMinHops {
		return config.Config{}, fmt.Errorf("app: chain %q needs at least %d hops", name, proxyChainMinHops)
	}

	hops := make([]config.Config, 0, len(ids))

	for i, id := range ids {
		cfg, err := a.decodeHop(id)
		if err != nil {
			return config.Config{}, fmt.Errorf("app: chain %q hop %d configuration not found — edit the chain before connecting", name, i+1)
		}

		hops = append(hops, *cfg)
	}

	exit := hops[len(hops)-1]
	exit.Chain = make([]*config.Config, 0, len(hops)-1)

	for i := 0; i < len(hops)-1; i++ {
		hop := hops[i]
		exit.Chain = append(exit.Chain, &hop)
	}

	// The synthetic configuration identifies as the CHAIN for
	// snapshots and diagnostics — never as one of its hops.
	exit.ID = chainID
	exit.Name = name

	return exit, nil
}

// decodeHop loads and decodes one hop configuration from the store.
// The JSON round-trip yields a deep copy (the Chain field is
// deliberately excluded — it is json:"-" — so a hop can never carry
// another chain's composition), and the id is pinned to the STORE
// fingerprint, never a chain id.
func (a *App) decodeHop(configID string) (*config.Config, error) {
	value, err := a.store.Get(configID)
	if err != nil {
		return nil, fmt.Errorf("app: configuration %s not found", configID)
	}

	cfg := &config.Config{}

	if err := json.Unmarshal(value, cfg); err != nil {
		return nil, fmt.Errorf("app: configuration %s is corrupt", configID)
	}

	cfg.ID = configID

	return cfg, nil
}

// loggingRecordChainCheck builds the structured log record for one
// chain check (honest outcomes only).
func loggingRecordChainCheck(chainID, name string, res tester.Result) logging.Record {
	return logging.Record{
		Level:     chainLogLevel(res.Working),
		Subsystem: "proxychain",
		Event:     "chain_check",
		Message:   chainCheckMessage(chainID, name, res),
		Fields: map[string]any{
			"chain_id":   chainID,
			"ok":         res.Working,
			"ping_ms":    res.PingMS,
			"backend":    res.Backend,
			"last_error": res.LastError,
		},
	}
}

func chainLogLevel(ok bool) logging.Level {
	if ok {
		return logging.LevelInfo
	}

	return logging.LevelWarn
}

func chainCheckMessage(chainID, name string, res tester.Result) string {
	if res.Working {
		return fmt.Sprintf("chain %s (%s): end-to-end check passed (%d ms)", chainID, name, res.PingMS)
	}

	return fmt.Sprintf("chain %s (%s): end-to-end check failed: %s", chainID, name, res.LastError)
}
