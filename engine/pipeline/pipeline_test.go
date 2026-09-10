package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	src "github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/store"
)

func subscriptionPayload(n int, prefix string) []byte {
	body := ""

	for i := 0; i < n; i++ {
		body += fmt.Sprintf(
			"%s://user%d@host%d.example.com:443?security=tls&type=ws#node%d\n",
			prefix, i, i, i)
	}

	return []byte(body)
}

type memSink struct {
	mu      sync.Mutex
	items   map[string]config.Config
	err     error
	batches int
}

func newMemSink() *memSink {
	return &memSink{items: make(map[string]config.Config)}
}

func (m *memSink) Persist(ctx context.Context, configs []config.Config) error {
	if m.err != nil {
		return m.err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.batches++

	for i := range configs {
		m.items[configs[i].Fingerprint()] = configs[i]
	}

	return nil
}

func TestPipelineEndToEnd(t *testing.T) {
	var servers []*httptest.Server

	for i := 0; i < 3; i++ {
		payload := subscriptionPayload(50, "vless")

		servers = append(servers, httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			})))
	}

	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()

	srcs := make([]SourceDef, 0, 3)

	for i, s := range servers {
		srcs = append(srcs, SourceDef{
			ID:      fmt.Sprintf("src-%d", i),
			Name:    fmt.Sprintf("Source %d", i),
			URL:     s.URL,
			Enabled: true,
		})
	}

	st := openPipelineStore(t)
	sink := NewStoreSink(st, 32)

	p := New(DefaultConfig(), nil)

	stats, hashes, err := p.Run(context.Background(), toSources(srcs), sink, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if stats.SourcesOK != 3 {
		t.Fatalf("sources ok = %d, want 3", stats.SourcesOK)
	}

	// 150 configs, but sources 1 and 2 carry identical payloads to
	// source 0, so dedup keeps 50.
	if stats.Discovered != 150 {
		t.Fatalf("discovered = %d, want 150", stats.Discovered)
	}

	if stats.Persisted != 50 {
		t.Fatalf("persisted = %d, want 50", stats.Persisted)
	}

	if stats.Duplicates != 100 {
		t.Fatalf("duplicates = %d, want 100", stats.Duplicates)
	}

	if len(hashes) != 3 {
		t.Fatalf("hashes = %d entries, want 3", len(hashes))
	}

	if st.Count() != 50 {
		t.Fatalf("store count = %d, want 50", st.Count())
	}

	// Second run: unchanged content must short-circuit.
	stats2, _, err := p.Run(context.Background(), toSources(srcs), sink, hashes)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if stats2.SourcesUnchanged != 3 {
		t.Fatalf("unchanged = %d, want 3", stats2.SourcesUnchanged)
	}

	if stats2.Persisted != 0 {
		t.Fatalf("second run persisted %d, want 0", stats2.Persisted)
	}
}

func TestPipelineSourceFailureIsolation(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(subscriptionPayload(20, "trojan"))
		}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
	defer bad.Close()

	srcs := []SourceDef{
		{ID: "good", URL: good.URL, Enabled: true},
		{ID: "bad", URL: bad.URL, Enabled: true},
	}

	st := openPipelineStore(t)
	p := New(DefaultConfig(), nil)

	stats, _, err := p.Run(context.Background(), toSources(srcs),
		NewStoreSink(st, 16), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if stats.SourcesOK != 1 || stats.SourcesFailed != 1 {
		t.Fatalf("ok=%d failed=%d, want 1/1",
			stats.SourcesOK, stats.SourcesFailed)
	}

	if stats.Persisted != 20 {
		t.Fatalf("persisted = %d, want 20", stats.Persisted)
	}
}

func TestPipelineCancellation(t *testing.T) {
	release := make(chan struct{})

	slow := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			<-release

			_, _ = w.Write(subscriptionPayload(5, "vless"))
		}))
	defer slow.Close()

	ctx, cancel := context.WithCancel(context.Background())

	srcs := []SourceDef{{ID: "slow", URL: slow.URL, Enabled: true}}

	go func() {
		// Give the fetch a moment to start, then cancel.
		cancel()
		close(release)
	}()

	st := openPipelineStore(t)
	p := New(DefaultConfig(), nil)

	_, _, err := p.Run(ctx, toSources(srcs), NewStoreSink(st, 8), nil)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestPipelineInvalidRecordsCounted(t *testing.T) {
	payload := "vless://user@host:443\n" + // valid
		"garbage-line\n" +
		"vless://user@bad:99999\n" // invalid port

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(payload))
		}))
	defer server.Close()

	srcs := []SourceDef{{ID: "mixed", URL: server.URL, Enabled: true}}

	st := openPipelineStore(t)
	p := New(DefaultConfig(), nil)

	stats, _, err := p.Run(context.Background(), toSources(srcs),
		NewStoreSink(st, 8), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if stats.Invalid == 0 {
		t.Fatal("invalid records not counted")
	}
}

func TestNilSinkRejected(t *testing.T) {
	p := New(DefaultConfig(), nil)

	_, _, err := p.Run(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("nil sink must be rejected")
	}
}

// --- helpers ---

// SourceDef mirrors source.Source without importing it in test bodies.
type SourceDef struct {
	ID      string
	Name    string
	URL     string
	Enabled bool
}

func toSources(defs []SourceDef) []src.Source {
	out := make([]src.Source, 0, len(defs))

	for _, d := range defs {
		out = append(out, src.Source{
			ID:      d.ID,
			Name:    d.Name,
			URL:     d.URL,
			Enabled: d.Enabled,
		})
	}

	return out
}

func openPipelineStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(store.Options{Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}

func BenchmarkPipelineRun(b *testing.B) {
	payload := subscriptionPayload(5000, "vless")

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(payload)
		}))
	defer server.Close()

	st, err := store.Open(store.Options{Path: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}

	defer st.Close()

	p := New(DefaultConfig(), nil)
	sink := NewStoreSink(st, 512)

	srcs := toSources([]SourceDef{
		{ID: "bench", URL: server.URL, Enabled: true},
	})

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		stats, _, err := p.Run(context.Background(), srcs, sink, nil)
		if err != nil {
			b.Fatal(err)
		}

		if stats.Persisted != 5000 {
			b.Fatalf("persisted %d", stats.Persisted)
		}
	}
}
