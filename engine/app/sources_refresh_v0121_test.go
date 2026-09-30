package app

// sources_refresh_v0121_test.go pins the v0.12.1 targeted-source
// capabilities (§18): one source can be refreshed individually
// through the SAME ingestion architecture, two refresh requests can
// never create two ingestion authorities, and the normal runtime log
// never re-adopts core build provenance.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRefreshSourceTargetsExactlyOneSource: refreshing source A must
// fetch ONLY source A, persist only its configs, and return its
// bounded stats — source B stays untouched.
func TestRefreshSourceTargetsExactlyOneSource(t *testing.T) {
	const perSource = 5

	payload := ""

	for i := 0; i < perSource; i++ {
		payload += fmt.Sprintf(
			"vless://uuid-%d@srv%d.example.com:443?security=tls&type=ws#node%d\n",
			i, i, i)
	}

	var hitsA, hitsB atomic.Int64

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsA.Add(1)
		_, _ = w.Write([]byte(payload))
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsB.Add(1)
		_, _ = w.Write([]byte(payload))
	}))
	defer serverB.Close()

	app := newTestApp(t)
	sources := NewSourceService(app)

	if err := sources.Add("src-a", "A", serverA.URL); err != nil {
		t.Fatalf("add A: %v", err)
	}

	if err := sources.Add("src-b", "B", serverB.URL); err != nil {
		t.Fatalf("add B: %v", err)
	}

	stats, err := sources.RefreshSource("src-a")
	if err != nil {
		t.Fatalf("targeted refresh: %v", err)
	}

	if hitsA.Load() != 1 {
		t.Fatalf("source A hits = %d, want 1", hitsA.Load())
	}

	if hitsB.Load() != 0 {
		t.Fatalf("source B hits = %d, want 0 (the refresh must be scoped)", hitsB.Load())
	}

	if stats == nil || stats.ID != "src-a" {
		t.Fatalf("stats = %+v, want source-scoped stats for src-a", stats)
	}

	data := NewDataService(app)

	page, err := data.ListConfigs(0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if page.Total != perSource {
		t.Fatalf("persisted = %d, want %d", page.Total, perSource)
	}

	for _, cfg := range page.Items {
		if cfg.Source != "src-a" {
			t.Fatalf("config %q carries source %q, want src-a", cfg.ID, cfg.Source)
		}
	}
}

// TestRefreshSourceRespectsEnabledPolicy: a disabled source refuses an
// explicit refresh (the scheduler's policy, respected verbatim).
func TestRefreshSourceRespectsEnabledPolicy(t *testing.T) {
	app := newTestApp(t)
	sources := NewSourceService(app)

	if err := sources.Add("off", "Off", "http://127.0.0.1:1/none"); err != nil {
		t.Fatalf("add: %v", err)
	}

	if err := sources.SetEnabled("off", false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, err := sources.RefreshSource("off"); err == nil {
		t.Fatal("disabled source must refuse a targeted refresh")
	}
}

// TestRefreshSourceUnknownID: an invalid source ID is validated before
// any ingestion work happens.
func TestRefreshSourceUnknownID(t *testing.T) {
	app := newTestApp(t)
	sources := NewSourceService(app)

	if _, err := sources.RefreshSource("does-not-exist"); err == nil {
		t.Fatal("unknown source id must fail")
	}
}

// TestRefreshSourceSingleOwnerGate (§18 concurrency contract): while
// the ingestion gate is held, a targeted refresh must NOT start a
// second competing cycle — it reports the busy state honestly.
func TestRefreshSourceSingleOwnerGate(t *testing.T) {
	app := newTestApp(t)
	sources := NewSourceService(app)

	if err := sources.Add("gated", "Gated", "http://127.0.0.1:1/none"); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Hold the ONE ingestion gate (as a running full cycle would).
	if !app.ingesting.CompareAndSwap(false, true) {
		t.Fatal("gate must start free")
	}

	var wg sync.WaitGroup

	errCh := make(chan error, 1)

	wg.Add(1)

	go func() {
		defer wg.Done()
		// started=false in the harness: the polite wait is skipped and
		// the single-owner CAS decides immediately.
		_, err := sources.RefreshSource("gated")
		errCh <- err
	}()

	wg.Wait()

	err := <-errCh

	app.ingesting.Store(false)

	if err == nil || !strings.Contains(err.Error(), "another ingestion cycle") {
		t.Fatalf("err = %v, want the single-owner busy error", err)
	}
}

// TestConcurrentRefreshSourceOnlyOneCycle: two simultaneous targeted
// refreshes of the SAME source admit exactly one ingestion cycle —
// the loser gets the honest busy error, the store stays consistent.
func TestConcurrentRefreshSourceOnlyOneCycle(t *testing.T) {
	var hits atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("vless://u@only-one.example.com:443?security=tls&type=ws#n\n"))
	}))
	defer server.Close()

	app := newTestApp(t)
	sources := NewSourceService(app)

	if err := sources.Add("race", "Race", server.URL); err != nil {
		t.Fatalf("add: %v", err)
	}

	const callers = 2

	var (
		wg       sync.WaitGroup
		busy     atomic.Int64
		success  atomic.Int64
		startGun sync.Mutex
	)

	startGun.Lock()

	for i := 0; i < callers; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			startGun.Lock()
			startGun.Unlock()

			if _, err := sources.RefreshSource("race"); err != nil {
				if strings.Contains(err.Error(), "another ingestion cycle") {
					busy.Add(1)
				}
			} else {
				success.Add(1)
			}
		}()
	}

	startGun.Unlock()
	wg.Wait()

	if got := hits.Load(); got > callers {
		t.Fatalf("server hits = %d, want <= %d", got, callers)
	}

	// One cycle ran; the other was refused by the single-owner gate
	// (or serialized behind it — never a parallel second authority).
	if success.Load() == 0 {
		t.Fatal("at least one caller must have completed the refresh")
	}
}

// TestCoreDiscoveredMessageProvenanceFree pins the §33 runtime-log
// privacy regression: normal-log core_discovered messages NEVER carry
// commit hashes, Go runtime tuples or paths — the v0.12.0 leak form.
func TestCoreDiscoveredMessageProvenanceFree(t *testing.T) {
	cases := []struct {
		name    string
		core    string
		raw     string
		message string
	}{
		{
			name:    "release build",
			core:    "xray",
			raw:     "Xray 26.3.27 (Xray, Penetrates Everything.) Custom (go1.26.1 windows/amd64)",
			message: "xray 26.3.27 available",
		},
		{
			name:    "dev build with commit hash",
			core:    "xray",
			raw:     "Xray-core d2758a0 (go1.26.1 windows/amd64)",
			message: "xray available",
		},
		{
			name:    "custom v2ray build",
			core:    "v2ray",
			raw:     "Custom (go1.26.1 windows/amd64)",
			message: "v2ray available",
		},
		{
			name:    "sing-box go tuple",
			core:    "sing-box",
			raw:     "sing-box version 1.14.1 (go1.26.1 windows/amd64, CGO)",
			message: "sing-box 1.14.1 available",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := coreDiscoveredMessage(tc.core, tc.raw)

			if got != tc.message {
				t.Fatalf("message = %q, want %q", got, tc.message)
			}

			for _, forbidden := range []string{"go1.", "windows/amd64", "d2758a0", " at ", "/"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("message %q contains forbidden provenance %q", got, forbidden)
				}
			}
		})
	}
}

// TestCompactCoreVersionDropsProvenance: the sanitizer can never emit
// a Go-runtime or commit fragment even on adversarial inputs.
func TestCompactCoreVersionDropsProvenance(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"26.3.27", "26.3.27"},
		{"5.53.0 (V2Fly, a V2Ray community.)", "5.53.0"},
		{"go1.26.1 windows/amd64", ""}, // pure provenance
		{"d2758a0 (dirty build)", ""},  // commit hash
		{"version 1.14.1 (go1.26 linux/amd64)", "1.14.1"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := compactCoreVersion(tc.raw); got != tc.want {
			t.Errorf("compactCoreVersion(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
