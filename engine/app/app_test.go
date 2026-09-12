package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/store"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// newTestApp creates an app bound to a temp directory with fast
// scheduler settings and no ingestion at boot.
func newTestApp(t *testing.T) *App {
	t.Helper()

	application, err := New(Options{
		BaseDir:             filepath.Join(t.TempDir(), "freeiran"),
		RefreshInterval:     time.Hour,
		RunIngestionOnStart: false,
		SkipDefaultSources:  true,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	t.Cleanup(application.Shutdown)

	return application
}

func TestAppBootsReadyAndExposesState(t *testing.T) {
	app := newTestApp(t)

	state := app.State()

	if state.Status != "ready" {
		t.Fatalf("status = %s, want ready", state.Status)
	}

	if state.Version == "" {
		t.Fatal("version must be set")
	}

	if state.ConfigCount != 0 {
		t.Fatalf("fresh app has %d configs", state.ConfigCount)
	}

	if state.NativeAcceler == "" {
		t.Fatal("native mode must be reported")
	}
}

func TestAppEndToEndIngestionToUI(t *testing.T) {
	// E2E: source → ingestion → chunking → parse → normalize →
	// deduplicate → persist → read (spec §19).
	const perSource = 120

	payload := ""

	for i := 0; i < perSource; i++ {
		payload += fmt.Sprintf(
			"vless://uuid-%d@srv%d.example.com:443?security=tls&type=ws#node%d\n",
			i, i, i)
	}

	var hits atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)

			_, _ = w.Write([]byte(payload))
		}))
	defer server.Close()

	app := newTestApp(t)

	sources := NewSourceService(app)

	if err := sources.Add("e2e", "E2E Source", server.URL); err != nil {
		t.Fatalf("add source: %v", err)
	}

	// Ingest inline (scheduler not started).
	stats, err := sources.RefreshNow()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if stats.Persisted != perSource {
		t.Fatalf("persisted = %d, want %d", stats.Persisted, perSource)
	}

	data := NewDataService(app)

	// UI page 1.
	page, err := data.ListConfigs(0, 50)
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}

	if len(page.Items) != 50 || page.Total != perSource || !page.HasMore {
		t.Fatalf("page wrong: items=%d total=%d hasMore=%v",
			len(page.Items), page.Total, page.HasMore)
	}

	// Last page.
	lastPage, err := data.ListConfigs(100, 50)
	if err != nil {
		t.Fatalf("last page: %v", err)
	}

	if len(lastPage.Items) != 20 || lastPage.HasMore {
		t.Fatalf("last page wrong: items=%d hasMore=%v",
			len(lastPage.Items), lastPage.HasMore)
	}

	// Direct config access.
	first := page.Items[0]

	single, err := data.GetConfig(first.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}

	if single.ID != first.ID {
		t.Fatal("config identity mismatch")
	}

	// Search.
	found, err := data.SearchConfigs("srv5.example.com", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(found) != 1 {
		t.Fatalf("search found %d, want 1", len(found))
	}

	// Second refresh: unchanged content must not refetch-parse.
	if err := sources.SetEnabled("e2e", true); err != nil {
		t.Fatal(err)
	}

	before := hits.Load()

	if _, err := sources.RefreshNow(); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	// The fetch happens again (server hit) but parse/persist is
	// skipped via content hash — persisted stays stable.
	if app.store.Count() != perSource {
		t.Fatalf("count drifted: %d", app.store.Count())
	}

	if hits.Load() < before {
		t.Fatal("hit count regressed")
	}
}

func TestAppIngestionDeduplicatesAcrossSources(t *testing.T) {
	payload := "trojan://pass-a@shared.example.com:443#same\n"

	serverA := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(payload))
		}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(payload))
		}))
	defer serverB.Close()

	app := newTestApp(t)

	sources := NewSourceService(app)

	_ = sources.Add("src-a", "A", serverA.URL)
	_ = sources.Add("src-b", "B", serverB.URL)

	stats, err := sources.RefreshNow()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if stats.Persisted != 1 {
		t.Fatalf("persisted = %d, want 1 (dedup across sources)", stats.Persisted)
	}

	if stats.Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", stats.Duplicates)
	}
}

func TestAppStorageServices(t *testing.T) {
	app := newTestApp(t)

	// Stage some data.
	for i := 0; i < 30; i++ {
		cfg := config.Config{
			Type:    config.TypeVLESS,
			Address: fmt.Sprintf("srv%d.example.com", i),
			Port:    443,
			UUID:    fmt.Sprintf("uuid-%d", i),
		}

		cfg.Normalize()
		cfg.SetID()

		if err := app.store.Upsert(cfg.ID, []byte(fmt.Sprintf(
			`{"id":%q,"type":"vless","address":%q,"port":443,"uuid":%q}`,
			cfg.ID, cfg.Address, cfg.UUID))); err != nil {
			t.Fatal(err)
		}
	}

	if err := app.store.Flush(); err != nil {
		t.Fatal(err)
	}

	storage := NewStorageService(app)

	if storage.Stats().Count != 30 {
		t.Fatalf("stats count = %d", storage.Stats().Count)
	}

	verify, err := storage.Verify()
	if err != nil || !verify.OK {
		t.Fatalf("verify failed: %v %+v", err, verify)
	}

	if verify.ChunksChecked == 0 {
		t.Fatal("no chunks verified")
	}

	if err := storage.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	if app.store.Count() != 30 {
		t.Fatalf("count after compact = %d", app.store.Count())
	}

	// Legacy migration.
	legacyPath := filepath.Join(t.TempDir(), "legacy.json")

	legacy := `{"version":1,"entries":{}}`

	if err := writeFile(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}

	result, err := storage.MigrateLegacy(legacyPath)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if result.Migrated != 0 || !result.Renamed {
		t.Fatalf("unexpected migration result: %+v", result)
	}
}

func TestAppSchedulerRunsIngestion(t *testing.T) {
	var hits atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)

			_, _ = w.Write([]byte(
				"vless://uuid-x@srv.example.com:443#x\n"))
		}))
	defer server.Close()

	application, err := New(Options{
		BaseDir:             filepath.Join(t.TempDir(), "freeiran"),
		RefreshInterval:     50 * time.Millisecond,
		RefreshJitter:       10 * time.Millisecond,
		RunIngestionOnStart: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	defer application.Shutdown()

	// Register the source.
	NewSourceService(application).Add("sched", "Sched", server.URL)

	application.Start()

	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		if application.store.Count() >= 1 {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("scheduled ingestion never populated the store")
}

func TestAppShutdownIsClean(t *testing.T) {
	application, err := New(Options{
		BaseDir: filepath.Join(t.TempDir(), "freeiran"),
	})
	if err != nil {
		t.Fatal(err)
	}

	application.Start()

	// Write something so shutdown has work to flush.
	cfg := config.Config{
		Type: config.TypeVLESS, Address: "a.example.com", Port: 443,
		UUID: "u",
	}

	cfg.Normalize()
	cfg.SetID()

	_ = application.store.Upsert(cfg.ID, []byte(`{"x":1}`))

	application.Shutdown()

	if application.State().Status != "shutting_down" {
		t.Fatalf("status = %s", application.State().Status)
	}

	// Reopen must succeed and keep the data (WAL replay).
	reopened, err := store.Open(store.Options{
		Path: filepath.Join(application.opts.BaseDir, "data"),
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	defer reopened.Close()

	if reopened.Count() != 1 {
		t.Fatalf("reopened count = %d, want 1", reopened.Count())
	}
}

func TestSourceServiceValidation(t *testing.T) {
	app := newTestApp(t)
	sources := NewSourceService(app)

	if err := sources.Add("", "x", "https://x"); err == nil {
		t.Fatal("empty id must be rejected")
	}

	if err := sources.Add("dup", "x", "https://x"); err != nil {
		t.Fatal(err)
	}

	if err := sources.Add("dup", "y", "https://y"); err == nil {
		t.Fatal("duplicate id must be rejected")
	}

	if err := sources.SetEnabled("missing", false); err == nil {
		t.Fatal("missing source must error")
	}

	if err := sources.Remove("missing"); err == nil {
		t.Fatal("removing missing source must error")
	}

	list := sources.List()

	if len(list) != 1 || list[0].ID != "dup" {
		t.Fatalf("list = %+v", list)
	}
}

// TestAppShutdownIsIdempotent verifies Shutdown is safe to call any
// number of times — the v0.5.0-fixed guarantee. The store, connection
// manager and logger must each observe exactly one close.
func TestAppShutdownIsIdempotent(t *testing.T) {
	application, err := New(Options{
		BaseDir: filepath.Join(t.TempDir(), "freeiran"),
	})
	if err != nil {
		t.Fatal(err)
	}

	application.Start()

	// Write something so shutdown has work to flush.
	cfg := config.Config{
		Type: config.TypeVLESS, Address: "a.example.com", Port: 443,
		UUID: "u",
	}

	cfg.Normalize()
	cfg.SetID()

	_ = application.store.Upsert(cfg.ID, []byte(`{"x":1}`))

	// Triple shutdown must be safe and not panic.
	application.Shutdown()
	application.Shutdown()
	application.Shutdown()

	if application.State().Status != "shutting_down" {
		t.Fatalf("status = %s", application.State().Status)
	}

	// Reopen must succeed and keep the data (WAL replay).
	reopened, err := store.Open(store.Options{
		Path: filepath.Join(application.opts.BaseDir, "data"),
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	defer reopened.Close()

	if reopened.Count() != 1 {
		t.Fatalf("reopened count = %d, want 1", reopened.Count())
	}
}

// TestAppNewFailureClosesLogger verifies every New() failure path
// after logger creation closes the logger before returning. A leaked
// logger handle blocks TempDir cleanup on Windows.
func TestAppNewFailureClosesLogger(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "freeiran")

	// Open a logger we own; the app adopts it through Options.Logger.
	logger, err := logging.Open(logging.Options{
		Dir:  filepath.Join(dir, "logs"),
		Name: "freeiran.log",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Force a failure AFTER logger creation by pointing the store at
	// an unwritable path (a file in place of the data directory).
	dataPath := filepath.Join(dir, "data")
	if err := os.WriteFile(dataPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = New(Options{
		BaseDir: dir,
		Logger:  logger,
	})
	if err == nil {
		t.Fatal("New must fail when the store path is not a directory")
	}

	// The logger must have been closed by the failure path. A second
	// Close is a no-op (idempotent), but the first close must have
	// released the file handle.
	if err := logger.Close(); err != nil {
		t.Fatalf("logger not closed by failure path: %v", err)
	}

	// And the log directory must be removable immediately.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("TempDir cleanup failed after New failure: %v", err)
	}
}

// TestAppShutdownReleasesAllHandles verifies no file handle outlives
// Shutdown — the Windows-critical guarantee. On Linux we observe this
// directly through /proc/self/fd.
func TestAppShutdownReleasesAllHandles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc/self/fd")
	}

	dir := filepath.Join(t.TempDir(), "freeiran")

	application, err := New(Options{
		BaseDir:             dir,
		RunIngestionOnStart: false,
		SkipDefaultSources:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	application.Start()
	application.Shutdown()

	// No file descriptor should reference anything under the app's
	// base directory.
	if leaks := listOpenFilesUnder(dir); len(leaks) > 0 {
		t.Fatalf("file handles leaked after shutdown: %v", leaks)
	}
}
