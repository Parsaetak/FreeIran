package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/database"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/tester"
)

type mockProbe struct {
	supported map[config.Type]bool
	result    tester.Result
	err       error
}

func (p *mockProbe) Supports(protocol config.Type) bool {
	if p == nil {
		return false
	}

	return p.supported[protocol]
}

func (p *mockProbe) Test(
	ctx context.Context,
	cfg config.Config,
) (tester.Result, error) {
	if err := ctx.Err(); err != nil {
		return tester.Result{}, err
	}

	result := p.result

	if result.TestedAt.IsZero() {
		result.TestedAt = time.Now().UTC()
	}

	return result, p.err
}

type mockCore struct {
	protocol config.Type
}

func (c *mockCore) Type() config.Type {
	return c.protocol
}

func (c *mockCore) Start(
	ctx context.Context,
	cfg config.Config,
) (core.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return &mockInstance{
		endpoint: cfg.Address,
	}, nil
}

type mockInstance struct {
	endpoint string
}

func (i *mockInstance) Endpoint() string {
	return i.endpoint
}

func (i *mockInstance) Close() error {
	return nil
}

func testVLESSConfiguration() string {
	return "vless://11111111-1111-1111-1111-111111111111@example.com:443" +
		"?type=tcp&security=tls#test"
}

func testSourceServer(t *testing.T, content string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(content))
	}))
}

func TestRunOnceWithoutDatabase(t *testing.T) {
	probe := &mockProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
		result: tester.Result{
			Working: true,
			Latency: 25 * time.Millisecond,
		},
	}

	engine := New(
		nil,
		tester.New(probe),
		nil,
	)

	server := testSourceServer(t, testVLESSConfiguration())
	defer server.Close()

	result, err := engine.RunOnce(
		context.Background(),
		[]source.Source{
			{
				ID:      "test",
				Name:    "Test",
				URL:     server.URL,
				Enabled: true,
			},
		},
	)
	if err != nil {
		t.Fatalf("RunOnce() failed: %v", err)
	}

	if result.Discovered != 1 {
		t.Fatalf("expected 1 discovered configuration, got %d", result.Discovered)
	}

	if result.Unique != 1 {
		t.Fatalf("expected 1 unique configuration, got %d", result.Unique)
	}

	if result.Tested != 1 {
		t.Fatalf("expected 1 tested configuration, got %d", result.Tested)
	}

	if result.Working != 1 {
		t.Fatalf("expected 1 working configuration, got %d", result.Working)
	}

	if result.Failed != 0 {
		t.Fatalf("expected 0 failed configurations, got %d", result.Failed)
	}

	if len(result.Configurations) != 1 {
		t.Fatalf(
			"expected 1 result configuration, got %d",
			len(result.Configurations),
		)
	}

	cfg := result.Configurations[0]

	if !cfg.Working {
		t.Fatal("expected configuration to be marked working")
	}

	if cfg.LatencyMS != 25 {
		t.Fatalf("expected latency 25ms, got %dms", cfg.LatencyMS)
	}

	if cfg.TestedAt == 0 {
		t.Fatal("expected TestedAt to be populated")
	}

	if result.FinishedAt.IsZero() {
		t.Fatal("expected FinishedAt to be populated")
	}

	if result.Duration < 0 {
		t.Fatal("expected non-negative duration")
	}
}

func TestRunOnceDeduplicatesConfigurations(t *testing.T) {
	probe := &mockProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
		result: tester.Result{
			Working: true,
		},
	}

	engine := New(nil, tester.New(probe), nil)

	server := testSourceServer(t, testVLESSConfiguration())
	defer server.Close()

	sources := []source.Source{
		{
			ID:      "source-a",
			Name:    "Source A",
			URL:     server.URL,
			Enabled: true,
		},
		{
			ID:      "source-b",
			Name:    "Source B",
			URL:     server.URL,
			Enabled: true,
		},
	}

	result, err := engine.RunOnce(context.Background(), sources)
	if err != nil {
		t.Fatalf("RunOnce() failed: %v", err)
	}

	if result.Discovered != 2 {
		t.Fatalf("expected 2 discovered configurations, got %d", result.Discovered)
	}

	if result.Unique != 1 {
		t.Fatalf("expected 1 unique configuration, got %d", result.Unique)
	}

	if result.Tested != 1 {
		t.Fatalf("expected 1 tested configuration, got %d", result.Tested)
	}

	if len(result.Configurations) != 1 {
		t.Fatalf(
			"expected 1 final configuration, got %d",
			len(result.Configurations),
		)
	}
}

func TestRunOnceHandlesPartialSourceFailure(t *testing.T) {
	probe := &mockProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
		result: tester.Result{
			Working: true,
		},
	}

	engine := New(nil, tester.New(probe), nil)

	server := testSourceServer(t, testVLESSConfiguration())
	defer server.Close()

	sources := []source.Source{
		{
			ID:      "working",
			Name:    "Working",
			URL:     server.URL,
			Enabled: true,
		},
		{
			ID:      "broken",
			Name:    "Broken",
			URL:     "://invalid-url",
			Enabled: true,
		},
	}

	result, err := engine.RunOnce(context.Background(), sources)

	if err == nil {
		t.Fatal("expected partial source failure")
	}

	if result.Discovered != 1 {
		t.Fatalf(
			"expected successful source to contribute 1 configuration, got %d",
			result.Discovered,
		)
	}

	if result.Unique != 1 {
		t.Fatalf("expected 1 unique configuration, got %d", result.Unique)
	}

	if result.Tested != 1 {
		t.Fatalf("expected successful configuration to be tested, got %d", result.Tested)
	}

	if result.Working != 1 {
		t.Fatalf("expected 1 working configuration, got %d", result.Working)
	}

	if !strings.Contains(err.Error(), "one or more sources failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunOnceRejectsUnsupportedRegistryProtocol(t *testing.T) {
	probe := &mockProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
		result: tester.Result{
			Working: true,
		},
	}

	registry := core.NewRegistry()

	if err := registry.Register(&mockCore{
		protocol: config.TypeVMess,
	}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	engine := New(
		nil,
		tester.New(probe),
		registry,
	)

	server := testSourceServer(t, testVLESSConfiguration())
	defer server.Close()

	result, err := engine.RunOnce(
		context.Background(),
		[]source.Source{
			{
				ID:      "test",
				URL:     server.URL,
				Enabled: true,
			},
		},
	)

	if err != nil {
		t.Fatalf("RunOnce() failed: %v", err)
	}

	if result.Tested != 1 {
		t.Fatalf("expected 1 tested configuration, got %d", result.Tested)
	}

	if result.Working != 0 {
		t.Fatalf("expected 0 working configurations, got %d", result.Working)
	}

	if result.Failed != 1 {
		t.Fatalf("expected 1 failed configuration, got %d", result.Failed)
	}

	if result.Configurations[0].Working {
		t.Fatal("unsupported configuration must not be marked working")
	}
}

func TestRunOncePersistsDatabase(t *testing.T) {
	probe := &mockProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
		result: tester.Result{
			Working: true,
			Latency: 10 * time.Millisecond,
		},
	}

	db, err := database.New(
		filepath.Join(t.TempDir(), "database.json"),
	)
	if err != nil {
		t.Fatalf("database.New() failed: %v", err)
	}

	engine := New(nil, tester.New(probe), nil)
	engine.SetDatabase(db)

	server := testSourceServer(t, testVLESSConfiguration())
	defer server.Close()

	result, err := engine.RunOnce(
		context.Background(),
		[]source.Source{
			{
				ID:      "test",
				URL:     server.URL,
				Enabled: true,
			},
		},
	)
	if err != nil {
		t.Fatalf("RunOnce() failed: %v", err)
	}

	if len(result.Configurations) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(result.Configurations))
	}

	id := result.Configurations[0].ID
	if id == "" {
		t.Fatal("expected configuration ID")
	}

	stored, ok := db.Get(id)
	if ok != nil {
	t.Fatalf("expected success, got: %v", ok)
}

	if stored == nil {
		t.Fatal("database returned nil configuration")
	}

	if stored.ID != id {
		t.Fatalf(
			"expected persisted ID %q, got %q",
			id,
			stored.ID,
		)
	}

	if !stored.Working {
		t.Fatal("expected persisted configuration to be working")
	}
}

func TestNewWithDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")

	engine, err := NewWithDatabase(
		nil,
		nil,
		nil,
		path,
	)
	if err != nil {
		t.Fatalf("NewWithDatabase() failed: %v", err)
	}

	if engine == nil {
		t.Fatal("expected engine")
	}

	if engine.Database == nil {
		t.Fatal("expected database to be attached")
	}
}

func TestRunOnceContextCancellation(t *testing.T) {
	engine := New(
		source.NewCollector(),
		tester.New(&mockProbe{
			supported: map[config.Type]bool{
				config.TypeVLESS: true,
			},
			result: tester.Result{
				Working: true,
			},
		}),
		nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := engine.RunOnce(
		ctx,
		[]source.Source{
			{
				ID:      "test",
				URL:     "http://127.0.0.1:1",
				Enabled: true,
			},
		},
	)

	if err == nil {
		t.Fatal("expected context cancellation error")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf(
			"expected context.Canceled, got %v",
			err,
		)
	}

	if !result.FinishedAt.IsZero() {
		t.Fatal("expected cancelled cycle to return before completion timestamp")
	}
}

func TestRunOnceNilTester(t *testing.T) {
	engine := New(nil, nil, nil)

	server := testSourceServer(t, testVLESSConfiguration())
	defer server.Close()

	result, err := engine.RunOnce(
		context.Background(),
		[]source.Source{
			{
				ID:      "test",
				URL:     server.URL,
				Enabled: true,
			},
		},
	)

	if err == nil {
		t.Fatal("expected tester configuration error")
	}

	if !strings.Contains(err.Error(), "tester is not configured") {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Unique != 1 {
		t.Fatalf("expected 1 unique configuration, got %d", result.Unique)
	}

	if result.FinishedAt.IsZero() {
		t.Fatal("expected FinishedAt to be populated")
	}
}
