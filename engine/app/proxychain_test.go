package app

// proxychain_test.go — v0.12.2 regression coverage for the proxy-chain
// feature on the EXISTING collections authority:
//
//   - collections schema v1 → v2 migration: user groups become
//     kind="user", nothing is lost, the next save rewrites v2;
//   - chain create/rename/delete/add/reorder/remove through the ONE
//     sidecar (atomic write, no second store);
//   - rejection of duplicates, missing configs, nested chains,
//     oversized chains and degenerate one-hop chains;
//   - the synthetic chain composition (exit + ordered hops, IDs only);
//   - CheckChain reports honest per-hop + end-to-end evidence.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func storeChainHop(t *testing.T, a *App, name string) string {
	return storeTestConfig(t, a, name)
}

// TestCollectionsV1MigratesToKindUser loads a real v1 sidecar and
// asserts every group surfaces as a user group afterwards (and the
// next save rewrites the document as version 2).
func TestCollectionsV1MigratesToKindUser(t *testing.T) {
	a := newTestApp(t)

	if err := os.MkdirAll(a.layout.Config, 0o700); err != nil {
		t.Fatal(err)
	}

	v1 := `{
  "version": 1,
  "favorites": ["fp-1"],
  "groups": [
    {"id": "g-1", "name": "Work", "config_ids": ["fp-a", "fp-b"], "created_at": 1700000000000},
    {"id": "g-2", "name": "Personal", "config_ids": [], "created_at": 1700000001000}
  ]
}`

	if err := os.WriteFile(a.collectionsPath(), []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := NewCollectionService(a)
	groups := svc.UserGroups()

	if len(groups) != 2 {
		t.Fatalf("user groups after migration = %d, want 2", len(groups))
	}

	if groups[0].Name != "Personal" || groups[1].Name != "Work" {
		t.Fatalf("migrated groups drifted: %+v", groups)
	}

	if got, err := svc.GroupMembers("g-1"); err != nil || len(got) != 2 {
		t.Fatalf("members lost in migration: %v (err=%v)", got, err)
	}

	// A mutation persists version 2 WITH the kind field.
	if _, err := svc.CreateUserGroup("Travel"); err != nil {
		t.Fatalf("create after migration: %v", err)
	}

	raw, err := os.ReadFile(a.collectionsPath())
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Version int `json:"version"`
		Groups  []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"groups"`
	}

	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.Version != 2 {
		t.Fatalf("sidecar version = %d, want 2", doc.Version)
	}

	for _, grp := range doc.Groups {
		if grp.Kind != "user" {
			t.Fatalf("group %s kind = %q, want user", grp.ID, grp.Kind)
		}
	}
}

// TestProxyChainCrudLifecycle walks create → rename → add hop →
// reorder → remove → details → delete through the shared sidecar.
func TestProxyChainCrudLifecycle(t *testing.T) {
	a := newTestApp(t)

	h1 := storeChainHop(t, a, "hop-one")
	h2 := storeChainHop(t, a, "hop-two")
	h3 := storeChainHop(t, a, "hop-three")

	svc := NewProxyChainService(a)

	created, err := svc.CreateProxyChain("Berlin route", []string{h1, h2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !strings.HasPrefix(created.ID, "pc-") {
		t.Fatalf("chain id %q lacks the pc- prefix", created.ID)
	}

	// Duplicate NAME refused (case-insensitive).
	if _, err := svc.CreateProxyChain("berlin ROUTE", []string{h1, h2}); err == nil {
		t.Fatal("duplicate chain name accepted")
	}

	if err := svc.AddHop(created.ID, h3, 0); err != nil {
		t.Fatalf("add hop: %v", err)
	}

	if err := svc.ReorderHop(created.ID, h3, 2); err != nil {
		t.Fatalf("reorder hop: %v", err)
	}

	details, err := svc.ProxyChainDetails(created.ID)
	if err != nil {
		t.Fatalf("details: %v", err)
	}

	if len(details.ConfigIDs) != 3 || details.ConfigIDs[2] != h3 {
		t.Fatalf("reorder lost: %+v", details.ConfigIDs)
	}

	if details.Preview != "hop-one → hop-two → hop-three" {
		t.Fatalf("preview = %q", details.Preview)
	}

	if !details.Usable {
		t.Fatal("chain with all hops present reported unusable")
	}

	if err := svc.RenameProxyChain(created.ID, "Berlin exit"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Removing below two hops refuses.
	if err := svc.RemoveHop(created.ID, h2); err != nil {
		t.Fatalf("remove hop: %v", err)
	}

	if err := svc.RemoveHop(created.ID, h3); err == nil {
		t.Fatal("removing below two hops accepted")
	}

	if err := svc.DeleteProxyChain(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := svc.ProxyChainDetails(created.ID); err == nil {
		t.Fatal("deleted chain still resolvable")
	}
}

// TestProxyChainRejections pins every documented invariant:
// no duplicate hops, no missing config ids, no nested chains, max 4
// hops, min 2 hops — each with an actionable error.
func TestProxyChainRejections(t *testing.T) {
	a := newTestApp(t)

	h1 := storeChainHop(t, a, "hop-one")
	h2 := storeChainHop(t, a, "hop-two")
	h3 := storeChainHop(t, a, "hop-three")
	h4 := storeChainHop(t, a, "hop-four")
	h5 := storeChainHop(t, a, "hop-five")

	svc := NewProxyChainService(a)

	if _, err := svc.CreateProxyChain("solo", []string{h1}); err == nil {
		t.Fatal("one-hop chain accepted")
	}

	if _, err := svc.CreateProxyChain("empty", nil); err == nil {
		t.Fatal("empty chain accepted")
	}

	if _, err := svc.CreateProxyChain("dup", []string{h1, h1}); err == nil {
		t.Fatal("duplicate hops accepted")
	}

	if _, err := svc.CreateProxyChain("missing", []string{h1, "fp-does-not-exist"}); err == nil {
		t.Fatal("missing configuration accepted as a hop")
	}

	if _, err := svc.CreateProxyChain("toolong", []string{h1, h2, h3, h4, h5}); err == nil {
		t.Fatal("five-hop chain accepted (max 4)")
	}

	// A chain id is never a valid hop (no nesting).
	chain, err := svc.CreateProxyChain("outer", []string{h1, h2})
	if err != nil {
		t.Fatalf("seed chain: %v", err)
	}

	if err := svc.AddHop(chain.ID, "pc-1", -1); err == nil {
		t.Fatal("nested chain accepted")
	}

	if err := svc.ValidateProxyChain([]string{h1, "pc-9"}); err == nil {
		t.Fatal("validate accepts a chain id as a hop")
	}

	// Deleting a configuration the chain references flips the chain
	// unusable (honest) instead of silently dropping the hop.
	details, err := svc.ProxyChainDetails(chain.ID)
	if err != nil {
		t.Fatalf("details: %v", err)
	}

	if !details.Usable {
		t.Fatal("fresh chain reported unusable")
	}

	if err := a.store.Delete(h1); err != nil {
		t.Fatalf("delete hop config: %v", err)
	}

	details, err = svc.ProxyChainDetails(chain.ID)
	if err != nil {
		t.Fatalf("details after delete: %v", err)
	}

	if details.Usable {
		t.Fatal("chain with a missing hop reported usable")
	}

	if details.Hops[0].Available {
		t.Fatal("missing hop reported available")
	}
}

// TestProxyChainConnectComposition proves buildChainConfig composes
// the synthetic EXIT configuration: exit data as the config, earlier
// hops in Chain, the CHAIN id/name on the synthetic record, and a
// composition fingerprint distinct from the exit's own.
func TestProxyChainConnectComposition(t *testing.T) {
	a := newTestApp(t)

	h1 := storeChainHop(t, a, "hop-one")
	h2 := storeChainHop(t, a, "hop-two")
	h3 := storeChainHop(t, a, "hop-three")

	svc := NewProxyChainService(a)

	chain, err := svc.CreateProxyChain("compose", []string{h1, h2, h3})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	synthetic, err := a.buildChainConfig(chain.ID)
	if err != nil {
		t.Fatalf("buildChainConfig: %v", err)
	}

	if synthetic.ID != chain.ID {
		t.Fatalf("synthetic id = %q, want the chain id", synthetic.ID)
	}

	if synthetic.Name != "compose" {
		t.Fatalf("synthetic name = %q", synthetic.Name)
	}

	// The exit is the LAST user-ordered hop.
	if synthetic.Address != "hop-three.example.org" {
		t.Fatalf("egress = %q, want hop-three", synthetic.Address)
	}

	if len(synthetic.Chain) != 2 {
		t.Fatalf("earlier hops = %d, want 2", len(synthetic.Chain))
	}

	if synthetic.Chain[0].Address != "hop-one.example.org" ||
		synthetic.Chain[1].Address != "hop-two.example.org" {
		t.Fatalf("hop order lost: %+v", synthetic.Chain)
	}

	if synthetic.Fingerprint() == synthetic.ChainFingerprint() {
		t.Fatal("composition fingerprint equals the plain exit fingerprint")
	}

	// The composition is in-memory only: the store gains no chain
	// record and the sidecar never copies configuration payloads.
	if _, err := a.store.Get(chain.ID); err == nil {
		t.Fatal("chain leaked into the configuration store")
	}

	raw, err := os.ReadFile(filepath.Join(a.layout.Config, "collections.json"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), "example.org") || strings.Contains(string(raw), "uuid") {
		t.Fatal("sidecar leaked configuration payload data")
	}
}

// TestProxyChainCheckChainHonesty runs CheckChain against the fake
// configuration set: the end-to-end verdict must reflect the probe
// result honestly (here: no cores available → failed with the real
// error, never a fabricated OK).
func TestProxyChainCheckChainHonesty(t *testing.T) {
	a := newTestApp(t)

	h1 := storeChainHop(t, a, "hop-one")
	h2 := storeChainHop(t, a, "hop-two")

	svc := NewProxyChainService(a)

	chain, err := svc.CreateProxyChain("check", []string{h1, h2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	result, err := svc.CheckChain(chain.ID)
	if err != nil {
		t.Fatalf("check chain: %v", err)
	}

	// A test app has no protocol cores installed: the e2e probe
	// cannot claim success.
	if result.EndToEnd.OK {
		t.Fatal("chain e2e reported OK with no cores — fabricated evidence")
	}

	if len(result.Hops) != 2 {
		t.Fatalf("hop evidence = %d, want 2", len(result.Hops))
	}

	// Untested hops report their real state: untested (TestedAt 0).
	for _, hop := range result.Hops {
		if hop.TestedAt != 0 && hop.Working {
			t.Fatalf("hop %d fabricated working state", hop.Position)
		}
	}
}
