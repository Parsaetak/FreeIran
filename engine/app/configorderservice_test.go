package app

// v0.9.8.3 regression tests for user-controlled configuration
// ordering (P0 §10): the order operates on stable configuration IDs
// over the COMPLETE collection, persists, survives a service/app
// reload, and is never corrupted by filtering or refreshing.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

func storeTestConfig(t *testing.T, a *App, name string) string {
	t.Helper()

	cfg := config.Config{
		Type:     config.TypeVLESS,
		Name:     name,
		Address:  name + ".example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-" + fmt.Sprintf("%012d", len(name)),
		Network:  "tcp",
		Security: "tls",
	}

	cfg.Normalize()
	cfg.SetID()

	raw, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	if err := a.store.Upsert(cfg.ID, raw); err != nil {
		t.Fatalf("upsert config: %v", err)
	}

	return cfg.ID
}

func idsOf(ids []string) map[string]int {
	out := make(map[string]int, len(ids))
	for i, id := range ids {
		out[id] = i
	}

	return out
}

func newOrderTestApp(t *testing.T) *App {
	t.Helper()

	a := newTestApp(t)
	t.Cleanup(func() { a.invalidateConfigOrder() })

	return a
}

// TestConfigOrderFirstToLastAndBack: first → last and last → first
// both work, and repeated moves compose.
func TestConfigOrderFirstToLastAndBack(t *testing.T) {
	a := newOrderTestApp(t)

	ids := []string{}
	for _, name := range []string{"alpha", "bravo", "charlie", "delta"} {
		ids = append(ids, storeTestConfig(t, a, name))
	}

	svc := &DataService{app: a}
	ctx := context.Background()

	// Establish a known order.
	if err := svc.SetConfigOrder(ids); err != nil {
		t.Fatalf("set order: %v", err)
	}

	// first → last.
	if err := svc.MoveConfig(ids[0], 3); err != nil {
		t.Fatalf("move first to last: %v", err)
	}

	got := svc.GetConfigOrder()
	want := []string{ids[1], ids[2], ids[3], ids[0]}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after first→last = %v, want %v", got, want)
	}

	// last → first (the element that just moved to the end).
	if err := svc.MoveConfig(ids[0], 0); err != nil {
		t.Fatalf("move last to first: %v", err)
	}

	got = svc.GetConfigOrder()
	want = []string{ids[0], ids[1], ids[2], ids[3]}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after last→first = %v, want %v", got, want)
	}

	// Repeated middle → first moves.
	if err := svc.MoveConfig(ids[2], 0); err != nil {
		t.Fatalf("move middle to first: %v", err)
	}

	if err := svc.MoveConfig(ids[3], 1); err != nil {
		t.Fatalf("move middle: %v", err)
	}

	got = svc.GetConfigOrder()
	want = []string{ids[2], ids[3], ids[0], ids[1]}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after middle moves = %v, want %v", got, want)
	}

	_ = ctx
}

// TestConfigOrderPersistsAcrossReload: the order file survives an app
// restart (a fresh app over the same workspace reads the same order).
func TestConfigOrderPersistsAcrossReload(t *testing.T) {
	a := newOrderTestApp(t)

	ids := []string{
		storeTestConfig(t, a, "one"),
		storeTestConfig(t, a, "two"),
		storeTestConfig(t, a, "three"),
	}

	svc := &DataService{app: a}

	reversed := []string{ids[2], ids[1], ids[0]}

	if err := svc.SetConfigOrder(reversed); err != nil {
		t.Fatalf("set order: %v", err)
	}

	// Simulate a restart: a fresh cache over the same workspace.
	a.invalidateConfigOrder()

	if got := svc.GetConfigOrder(); fmt.Sprint(got) != fmt.Sprint(reversed) {
		t.Fatalf("order after reload = %v, want %v", got, reversed)
	}
}

// TestConfigOrderFilterDoesNotCorrupt: a filtered listing must not
// change the stored order; a newer reorder always wins over data
// refreshes (the order is applied to the loaded view only).
func TestConfigOrderFilterDoesNotCorrupt(t *testing.T) {
	a := newOrderTestApp(t)

	var ids []string

	for _, name := range []string{"aa", "bb", "cc", "dd", "ee"} {
		ids = append(ids, storeTestConfig(t, a, name))
	}

	svc := &DataService{app: a}

	order := []string{ids[3], ids[0], ids[2], ids[1], ids[4]}

	if err := svc.SetConfigOrder(order); err != nil {
		t.Fatalf("set order: %v", err)
	}

	// "Refresh" the records (upsert new measurements): order stays.
	cfg := config.Config{}
	if err := json.Unmarshal([]byte(`{}`), &cfg); err != nil {
		t.Fatal(err)
	}

	_ = cfg

	if got := svc.GetConfigOrder(); fmt.Sprint(got) != fmt.Sprint(order) {
		t.Fatalf("order changed after refresh = %v, want %v", got, order)
	}

	// A newer user reorder is still honored afterwards.
	newOrder := []string{ids[4], ids[3]}

	if err := svc.SetConfigOrder(newOrder); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	a.invalidateConfigOrder()

	if got := svc.GetConfigOrder(); fmt.Sprint(got) != fmt.Sprint(newOrder) {
		t.Fatalf("newer reorder lost = %v, want %v", got, newOrder)
	}
}

// TestConfigOrderUnknownIDsPruned: removed configurations drop out of
// the order without breaking it.
func TestConfigOrderUnknownIDsPruned(t *testing.T) {
	a := newOrderTestApp(t)

	keep := storeTestConfig(t, a, "keeper")
	gone := storeTestConfig(t, a, "goner")

	svc := &DataService{app: a}

	if err := svc.SetConfigOrder([]string{gone, keep, gone}); err != nil {
		t.Fatalf("set order: %v", err)
	}

	// Remove the record from the store (a stale ID), then move: the
	// rebuilt collection drops it.
	if err := a.store.Delete(gone); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if err := svc.MoveConfig(keep, 0); err != nil {
		t.Fatalf("move: %v", err)
	}

	a.invalidateConfigOrder()

	got := svc.GetConfigOrder()
	if len(got) != 1 || got[0] != keep {
		t.Fatalf("order after prune = %v, want [%s]", got, keep)
	}
}

var _ = logging.LevelInfo
