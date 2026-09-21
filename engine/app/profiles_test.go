// profiles_test.go — regression coverage for the v0.9.11 Connection
// Profiles feature (P2 §18).
//
// Contracts under test:
//
//   - lifecycle: create / edit / duplicate / rename / delete /
//     set default — every mutation persists atomically and survives a
//     restart (a fresh App on the same workspace);
//   - store isolation: profiles reference configuration IDs; deleting
//     a profile never deletes a configuration and a deleted
//     configuration has its profile references pruned safely;
//   - activation: runs through the ONE settings path (the live
//     connection manager observes ports immediately), supports Auto,
//     Configurations and provider (Tor/Psiphon) modes and the
//     recovery preference, and NEVER bypasses trust/verification —
//     activation is a preference change, not a connection;
//   - schema: the versioned sidecar refuses a future schema without
//     corrupting it (the forward-migration point);
//   - concurrency: concurrent List + SetActive never races or
//     double-applies.
package app

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

// profilesService is the bound service under test.
func profilesService(a *App) *ProfileService {
	return NewProfileService(a)
}

// ptrBool helper for the tri-state recovery preference.
func ptrBool(v bool) *bool {
	return &v
}

// TestProfileLifecycleAndPersistence covers create → edit → duplicate
// → rename → set default → delete and the full restart persistence of
// every marker (active, default, preferences).
func TestProfileLifecycleAndPersistence(t *testing.T) {
	a := newTestApp(t)

	configID := storeTestConfig(t, a, "profile-config")

	svc := profilesService(a)

	// --- create -----------------------------------------------------
	created, err := svc.Create(ProfileSpec{
		Name:             "Home",
		Mode:             ProviderModeConfigs,
		ConfigID:         configID,
		PreferredBackend: "xray",
		LocalSocksPort:   10808,
		LocalHTTPPort:    10809,
		AutoRecovery:     ptrBool(false),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if created.ID == "" || created.CreatedAt == 0 {
		t.Fatalf("created profile carries no stable id/timestamp: %+v", created)
	}

	if created.ConfigName != "profile-config" || !created.ConfigAvailable {
		t.Fatalf("created profile did not resolve its configuration: %+v", created)
	}

	// Duplicate names are rejected (case-insensitive).
	if _, err := svc.Create(ProfileSpec{Name: "home", Mode: ProviderModeAuto}); err == nil {
		t.Fatal("duplicate profile name accepted")
	}

	// Invalid modes, backends and ports are rejected.
	if _, err := svc.Create(ProfileSpec{Name: "Bad", Mode: "tun"}); err == nil {
		t.Fatal("invalid mode accepted")
	}

	if _, err := svc.Create(ProfileSpec{Name: "Bad", Mode: ProviderModeAuto, PreferredBackend: "zapret"}); err == nil {
		t.Fatal("invalid preferred backend accepted")
	}

	if _, err := svc.Create(ProfileSpec{Name: "Bad", Mode: ProviderModeAuto, LocalSocksPort: 80}); err == nil {
		t.Fatal("privileged port accepted")
	}

	if _, err := svc.Create(ProfileSpec{Name: "", Mode: ProviderModeAuto}); err == nil {
		t.Fatal("empty name accepted")
	}

	// --- edit -------------------------------------------------------
	updated, err := svc.Update(created.ID, ProfileSpec{
		Name:           "Home (edited)",
		Mode:           ProviderModeAuto,
		LocalSocksPort: 10810,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if updated.Mode != ProviderModeAuto || updated.ConfigID != "" || updated.LocalSocksPort != 10810 {
		t.Fatalf("update did not replace the editable fields: %+v", updated)
	}

	// --- duplicate --------------------------------------------------
	dup, err := svc.Duplicate(created.ID)
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}

	if dup.ID == created.ID || dup.Active {
		t.Fatalf("duplicate shares identity with the source: %+v", dup)
	}

	if dup.Name != "Home (edited) (copy)" {
		t.Fatalf("duplicate name = %q", dup.Name)
	}

	// A second duplicate disambiguates.
	dup2, err := svc.Duplicate(created.ID)
	if err != nil {
		t.Fatalf("duplicate #2: %v", err)
	}

	if dup2.Name != "Home (edited) (copy 2)" {
		t.Fatalf("second duplicate name = %q", dup2.Name)
	}

	// --- rename -----------------------------------------------------
	renamed, err := svc.Rename(dup.ID, "Travel")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}

	if renamed.Name != "Travel" || renamed.ID != dup.ID {
		t.Fatalf("rename changed identity: %+v", renamed)
	}

	// --- set default ------------------------------------------------
	if err := svc.SetDefault(created.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}

	views := svc.List()

	if len(views) != 3 {
		t.Fatalf("profile count = %d, want 3", len(views))
	}

	// Deterministic ordering: case-insensitive by name — "Home
	// (edited)" is a prefix of "Home (edited) (copy 2)", "Travel"
	// sorts last.
	if views[0].Name != "Home (edited)" || views[1].Name != "Home (edited) (copy 2)" || views[2].Name != "Travel" {
		t.Fatalf("list ordering = %v / %v / %v", views[0].Name, views[1].Name, views[2].Name)
	}

	if !views[0].Default {
		t.Fatalf("default flag not rendered: %+v", views[0])
	}

	// --- delete -----------------------------------------------------
	// Deleting the default clears the marker; the store keeps the
	// configuration (store isolation).
	if err := svc.Delete(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := a.store.Get(configID); err != nil {
		t.Fatalf("profile delete removed the underlying configuration: %v", err)
	}

	views = svc.List()

	if len(views) != 2 {
		t.Fatalf("profile count after delete = %d, want 2", len(views))
	}

	for _, view := range views {
		if view.Default {
			t.Fatalf("default marker survived the delete: %+v", view)
		}
	}

	if err := svc.Delete(dup.ID); err != nil {
		t.Fatalf("delete #2: %v", err)
	}

	if err := svc.Delete("p-999"); err == nil {
		t.Fatal("delete of an unknown profile succeeded")
	}

	// --- restart persistence ----------------------------------------
	// A fresh App on the SAME workspace must load the remaining
	// profile with its markers intact.
	restarted := restartApp(t, a)

	rsvc := profilesService(restarted)

	views = rsvc.List()

	if len(views) != 1 {
		t.Fatalf("restarted profile count = %d, want 1", len(views))
	}

	if views[0].ID != dup2.ID || views[0].Name != "Home (edited) (copy 2)" || views[0].Mode != ProviderModeAuto {
		t.Fatalf("restarted profile drifted: %+v", views[0])
	}

	if active, ok, _ := rsvc.Active(); ok {
		t.Fatalf("restarted app reports a stale active profile: %+v", active)
	}
}

// TestProfileActivationAppliesPreferencesThroughOnePath covers
// SetActive for the Auto, Configurations and provider modes: the
// preferences land in the live settings snapshot AND the live
// connection manager (ports), no connection starts, and no
// verification/trust boundary is touched.
func TestProfileActivationAppliesPreferencesThroughOnePath(t *testing.T) {
	a := newTestApp(t)

	configID := storeTestConfig(t, a, "activate-config")

	svc := profilesService(a)

	auto, err := svc.Create(ProfileSpec{
		Name:           "Privacy",
		Mode:           ProviderModeAuto,
		LocalSocksPort: 10808,
		LocalHTTPPort:  10809,
		AutoRecovery:   ptrBool(true), // recovery explicitly enabled
	})
	if err != nil {
		t.Fatalf("create auto profile: %v", err)
	}

	configs, err := svc.Create(ProfileSpec{
		Name:             "Work",
		Mode:             ProviderModeConfigs,
		ConfigID:         configID,
		PreferredBackend: "sing-box",
	})
	if err != nil {
		t.Fatalf("create configs profile: %v", err)
	}

	tor, err := svc.Create(ProfileSpec{
		Name: "Onion",
		Mode: ProviderModeTor,
	})
	if err != nil {
		t.Fatalf("create provider profile: %v", err)
	}

	// Baseline: no profile active, defaults in place.
	if _, ok, _ := svc.Active(); ok {
		t.Fatal("fresh app reports an active profile")
	}

	// --- Auto profile ------------------------------------------------
	applied, err := svc.SetActive(auto.ID)
	if err != nil {
		t.Fatalf("activate auto: %v", err)
	}

	if !applied.Active {
		t.Fatalf("activation did not set the flag: %+v", applied)
	}

	settings := a.currentSettings()

	if settings.ProviderMode != ProviderModeAuto ||
		settings.LocalSocksPort != 10808 || settings.LocalHTTPPort != 10809 {
		t.Fatalf("auto profile preferences not applied: %+v", settings)
	}

	if settings.DisableAutoRecovery {
		t.Fatal("auto recovery tri-state false must enable recovery (not disable it)")
	}

	// The live connection manager must observe the ports IMMEDIATELY
	// (applySettings routes them on save — no second mechanism).
	socks, http := a.connMgr.LocalPorts()

	if socks != 10808 || http != 10809 {
		t.Fatalf("live manager ports = %d/%d, want 10808/10809", socks, http)
	}

	// Activation is a preference change, never a connection: the
	// engine state stays disconnected and nothing bypassed the
	// verified state machine.
	if state := a.connMgr.State(); state.Active() {
		t.Fatalf("activation started a session (state %s) — profiles bypassed the engine", state)
	}

	// --- Configurations profile -------------------------------------
	if _, err := svc.SetActive(configs.ID); err != nil {
		t.Fatalf("activate configs: %v", err)
	}

	settings = a.currentSettings()

	if settings.ProviderMode != ProviderModeConfigs ||
		settings.PreferredBackend != "sing-box" ||
		settings.LocalSocksPort != 0 || settings.LocalHTTPPort != 0 {
		t.Fatalf("configs profile preferences not applied: %+v", settings)
	}

	// --- Provider profile -------------------------------------------
	if _, err := svc.SetActive(tor.ID); err != nil {
		t.Fatalf("activate provider: %v", err)
	}

	if got := a.currentSettings().ProviderMode; got != ProviderModeTor {
		t.Fatalf("provider mode = %q, want tor", got)
	}

	// The provider MODE is a preference; the provider session itself
	// still goes through providerService.Connect with its install
	// safety and verification gate. Activation must not start one.
	if a.connMgr.State().Active() {
		t.Fatal("provider profile activation started a session")
	}

	// Active marker survives a restart.
	restarted := restartApp(t, a)

	active, ok, err := profilesService(restarted).Active()
	if err != nil || !ok {
		t.Fatalf("active marker lost after restart (ok=%v, err=%v)", ok, err)
	}

	if active.ID != tor.ID {
		t.Fatalf("active profile after restart = %s, want %s", active.ID, tor.ID)
	}
}

// TestProfileDefaultAppliedAtBoot proves the startup-profile contract:
// the default profile's preferences are in effect on a fresh boot
// (in memory, without rewriting settings.json behind the user).
func TestProfileDefaultAppliedAtBoot(t *testing.T) {
	a := newTestApp(t)

	svc := profilesService(a)

	travel, err := svc.Create(ProfileSpec{
		Name:           "Travel",
		Mode:           ProviderModeConfigs,
		LocalSocksPort: 10810,
		AutoRecovery:   ptrBool(true),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := svc.SetDefault(travel.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}

	restarted := restartApp(t, a)

	settings := restarted.currentSettings()

	if settings.ProviderMode != ProviderModeConfigs || settings.LocalSocksPort != 10810 {
		t.Fatalf("default profile not applied at boot: %+v", settings)
	}

	if settings.DisableAutoRecovery {
		t.Fatal("recovery preference from the default profile not applied")
	}

	if got := restarted.currentSettings().LocalHTTPPort; got != 0 {
		t.Fatalf("unexpected http port: %d", got)
	}

	// The default marker persists for the NEXT boot as well.
	views := profilesService(restarted).List()

	if len(views) != 1 || !views[0].Default {
		t.Fatalf("default marker lost after restart: %+v", views)
	}
}

// TestProfileInvalidConfigReferenceCoversPruneAndRefusal proves the
// store-isolation contract in BOTH directions: a deleted
// configuration is reported honestly, activation of a configs
// profile whose configuration vanished fails loudly instead of
// silently connecting elsewhere, and the dangling reference is
// pruned persistently at boot.
func TestProfileInvalidConfigReferenceCoversPruneAndRefusal(t *testing.T) {
	a := newTestApp(t)

	configID := storeTestConfig(t, a, "vanishing-config")

	svc := profilesService(a)

	profile, err := svc.Create(ProfileSpec{
		Name:     "Work",
		Mode:     ProviderModeConfigs,
		ConfigID: configID,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The configuration disappears (the store is authoritative).
	if err := a.store.Delete(configID); err != nil {
		t.Fatalf("delete config: %v", err)
	}

	// Listing reports availability honestly without inventing data.
	views := svc.List()

	if len(views) != 1 || views[0].ConfigID != configID {
		t.Fatalf("view drifted: %+v", views)
	}

	if views[0].ConfigAvailable || views[0].ConfigName != "" {
		t.Fatalf("unavailable configuration rendered as available: %+v", views[0])
	}

	// Activation must refuse LOUDLY (no silent connect-elsewhere, no
	// bypass of configuration validation).
	if _, err := svc.SetActive(profile.ID); err == nil {
		t.Fatal("activation with a missing configuration succeeded — profiles bypassed validation")
	}

	// The active marker must NOT have moved.
	if _, ok, _ := svc.Active(); ok {
		t.Fatal("failed activation marked the profile active")
	}

	// A boot on the same workspace prunes the dangling reference
	// persistently (the "configuration deleted" cleanup contract).
	restarted := restartApp(t, a)

	views = profilesService(restarted).List()

	if len(views) != 1 || views[0].ConfigID != "" {
		t.Fatalf("dangling reference not pruned at boot: %+v", views)
	}

	// After the prune, activation succeeds WITHOUT the stale
	// reference (the mode preference still applies).
	if _, err := profilesService(restarted).SetActive(profile.ID); err != nil {
		t.Fatalf("activate after prune: %v", err)
	}

	if got := restarted.currentSettings().ProviderMode; got != ProviderModeConfigs {
		t.Fatalf("mode after pruned activation = %q, want configs", got)
	}
}

// TestProfileSchemaMigrationForward refuses a future schema version
// without rewriting the file (the documented migration point) and
// keeps the app fully functional.
func TestProfileSchemaMigrationForward(t *testing.T) {
	a := newTestApp(t)

	future := map[string]any{
		"version":            99,
		"profiles":           []map[string]any{{"id": "p-future", "name": "Future", "mode": "auto"}},
		"active_profile_id":  "p-future",
		"default_profile_id": "p-future",
	}

	raw, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}

	// Write through the same helper the sidecar fixtures use.
	if err := writeFile(a.profilesPath(), string(raw)); err != nil {
		t.Fatalf("write future sidecar: %v", err)
	}

	svc := profilesService(a)

	if views := svc.List(); len(views) != 0 {
		t.Fatalf("future schema produced profiles: %+v", views)
	}

	// The future file is untouched for the newer version to migrate.
	stored, err := os.ReadFile(a.profilesPath())
	if err != nil {
		t.Fatalf("future sidecar was removed: %v", err)
	}

	var doc map[string]any

	if err := json.Unmarshal(stored, &doc); err != nil {
		t.Fatalf("sidecar corrupted: %v", err)
	}

	if version, ok := doc["version"].(float64); !ok || int(version) != 99 {
		t.Fatalf("future schema version rewritten: %v", doc["version"])
	}
}

// TestProfileConcurrentListAndActivation pins the concurrency
// contract: concurrent List (refresh) and SetActive never race, never
// double-apply, and the final state is one coherently applied
// profile.
func TestProfileConcurrentListAndActivation(t *testing.T) {
	a := newTestApp(t)

	svc := profilesService(a)

	first, err := svc.Create(ProfileSpec{Name: "First", Mode: ProviderModeAuto, LocalSocksPort: 10811})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}

	second, err := svc.Create(ProfileSpec{Name: "Second", Mode: ProviderModeTor, LocalSocksPort: 10812})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			if i%4 == 0 {
				_, _ = svc.SetActive(first.ID)

				return
			}

			if i%4 == 1 {
				_, _ = svc.SetActive(second.ID)

				return
			}

			if i%4 == 2 {
				_ = svc.List()

				return
			}

			_, _, _ = svc.Active()
		}(i)
	}

	wg.Wait()

	// One coherently applied profile: the live settings match exactly
	// one of the two profiles and the active marker names it.
	active, ok, err := svc.Active()
	if err != nil || !ok {
		t.Fatalf("no active profile after concurrent activation (ok=%v, err=%v)", ok, err)
	}

	settings := a.currentSettings()

	switch active.ID {
	case first.ID:
		if settings.LocalSocksPort != 10811 || settings.ProviderMode != ProviderModeAuto {
			t.Fatalf("settings drifted from the active profile: %+v / %+v", active, settings)
		}
	case second.ID:
		if settings.LocalSocksPort != 10812 || settings.ProviderMode != ProviderModeTor {
			t.Fatalf("settings drifted from the active profile: %+v / %+v", active, settings)
		}
	default:
		t.Fatalf("unknown active profile %s", active.ID)
	}
}

// TestProfileDuplicateConfigReferencesNeverDuplicated proves profiles
// reference EXISTING configuration records: two profiles may point at
// the same configuration ID, the store carries exactly one record,
// and nothing about the configuration is copied into the sidecar.
func TestProfileDuplicateConfigReferencesNeverDuplicated(t *testing.T) {
	a := newTestApp(t)

	configID := storeTestConfig(t, a, "shared-config")

	svc := profilesService(a)

	for _, name := range []string{"Home", "Work"} {
		if _, err := svc.Create(ProfileSpec{
			Name: name, Mode: ProviderModeConfigs, ConfigID: configID,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	if got := a.store.Count(); got != 1 {
		t.Fatalf("store record count = %d, want 1 (profiles must never duplicate configurations)", got)
	}

	raw, err := os.ReadFile(a.profilesPath())
	if err != nil {
		t.Fatal(err)
	}

	// The sidecar carries no credential fields and no configuration
	// record — only the reference ids.
	for _, forbidden := range []string{"uuid", "password", "11111111-1111"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("sidecar carries forbidden material %q: %s", forbidden, raw)
		}
	}
}

// TestProviderModeConstantsAgree keeps the profile modes and the
// provider service modes a single vocabulary (the same constants the
// Quick Connect provider selector uses — no second mode system).
func TestProviderModeConstantsAgree(t *testing.T) {
	if len(AllProviderModes) != 4 {
		t.Fatalf("provider modes = %v", AllProviderModes)
	}

	known := map[string]bool{
		ProviderModeAuto:    true,
		ProviderModeConfigs: true,
		ProviderModeTor:     true,
		ProviderModePsiphon: true,
	}

	for _, mode := range AllProviderModes {
		if !known[mode] {
			t.Fatalf("AllProviderModes carries %q which is not a known mode", mode)
		}
	}

	// Every profile mode is one of the provider modes (and renders).
	for _, mode := range []string{
		ProviderModeAuto, ProviderModeConfigs, ProviderModeTor, ProviderModePsiphon,
	} {
		if ProviderModeLabel(mode) == "" {
			t.Fatalf("mode %q renders empty", mode)
		}
	}
}

// ---- test helpers -----------------------------------------------------

// restartApp shuts the current app down and boots a fresh App on the
// SAME workspace (the restart-persistence proof).
func restartApp(t *testing.T, a *App) *App {
	t.Helper()

	dir := a.opts.BaseDir

	a.Shutdown()

	restarted, err := New(Options{
		BaseDir:                 dir,
		RefreshInterval:         0,
		RunIngestionOnStart:     false,
		SkipDefaultSources:      true,
		SkipConnectVerification: true,
	})
	if err != nil {
		t.Fatalf("restart app: %v", err)
	}

	t.Cleanup(restarted.Shutdown)

	return restarted
}
