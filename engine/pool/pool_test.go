package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

type mockTester struct {
	mu      sync.Mutex
	calls   int
	results map[string]error
}

func (m *mockTester) Test(ctx context.Context, cfg *config.Config) error {
	m.mu.Lock()
	m.calls++
	err := m.results[cfg.ID]
	m.mu.Unlock()

	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func testConfig(id string) *config.Config {
	cfg := &config.Config{
		ID:      id,
		Address: "example.com",
		Port:    443,
	}

	return cfg
}

func TestNewPool(t *testing.T) {
	p := New(2)

	if p == nil {
		t.Fatal("expected non-nil pool")
	}
}

func TestNewPoolRejectsInvalidWorkerCount(t *testing.T) {
	p := New(0)

	if p == nil {
		t.Fatal("expected pool to normalize invalid worker count")
	}
}

func TestSubmit(t *testing.T) {
	p := New(2)

	err := p.Submit(testConfig("one"))
	if err != nil {
		t.Fatalf("Submit() failed: %v", err)
	}
}

func TestSubmitRejectsNilConfiguration(t *testing.T) {
	p := New(1)

	if err := p.Submit(nil); err == nil {
		t.Fatal("expected nil configuration error")
	}
}

func TestSubmitAfterClose(t *testing.T) {
	p := New(1)

	if err := p.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	if err := p.Submit(testConfig("closed")); err == nil {
		t.Fatal("expected Submit() to fail after Close()")
	}
}

func TestRunProcessesConfigurations(t *testing.T) {
	p := New(2)

	configs := []*config.Config{
		testConfig("one"),
		testConfig("two"),
		testConfig("three"),
	}

	var mu sync.Mutex
	processed := make(map[string]bool)

	tester := &mockTester{
		results: make(map[string]error),
	}

	p.SetTester(func(ctx context.Context, cfg *config.Config) error {
		if err := tester.Test(ctx, cfg); err != nil {
			return err
		}

		mu.Lock()
		processed[cfg.ID] = true
		mu.Unlock()

		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := p.Run(ctx, configs); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}

	for _, cfg := range configs {
		if !processed[cfg.ID] {
			t.Fatalf("configuration %q was not processed", cfg.ID)
		}
	}
}

func TestRunPropagatesTesterError(t *testing.T) {
	expected := errors.New("probe failed")

	p := New(1)

	p.SetTester(func(context.Context, *config.Config) error {
		return expected
	})

	err := p.Run(context.Background(), []*config.Config{
		testConfig("failure"),
	})

	if !errors.Is(err, expected) {
		t.Fatalf("expected %v, got %v", expected, err)
	}
}

func TestRunRejectsNilContext(t *testing.T) {
	p := New(1)

	err := p.Run(nil, []*config.Config{
		testConfig("one"),
	})

	if err == nil {
		t.Fatal("expected nil context error")
	}
}

func TestRunContextCancellation(t *testing.T) {
	p := New(1)

	started := make(chan struct{})

	p.SetTester(func(ctx context.Context, cfg *config.Config) error {
		close(started)

		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		done <- p.Run(ctx, []*config.Config{
			testConfig("one"),
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

func TestRunSkipsNilConfigurations(t *testing.T) {
	p := New(1)

	calls := 0

	p.SetTester(func(context.Context, *config.Config) error {
		calls++
		return nil
	})

	err := p.Run(context.Background(), []*config.Config{
		nil,
		testConfig("valid"),
	})

	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}

	if calls != 1 {
		t.Fatalf("expected one processed configuration, got %d", calls)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	p := New(1)

	if err := p.Close(); err != nil {
		t.Fatalf("first Close() failed: %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("second Close() failed: %v", err)
	}
}
