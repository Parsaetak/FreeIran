package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/database"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/tester"
)

func testEngineConfig() config.Config {
	return config.Config{
		Type:    config.ProtocolVLESS,
		Address: "127.0.0.1",
		Port:    443,
	}
}

func testEngineSource(t *testing.T, cfg config.Config) source.Source {
	t.Helper()

	return source.Source{
		Name:    "test",
		URL:     "https://example.com/configs",
		Enabled: true,
	}
}

func TestRunOnceWithoutDatabase(t *testing.T) {
	engine := New(nil, nil, nil)

	result, err := engine.RunOnce(context.Background(), nil)
	if err == nil {
		t.Fatal("expected tester configuration error")
	}

	if result.FinishedAt.IsZero() {
		t.Fatal("expected FinishedAt to be set")
	}
}

func TestSetDatabase(t *testing.T) {
	engine := New(nil, nil, nil)

	db, err := database.New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatalf("database.New() failed: %v", err)
	}

	engine.SetDatabase(db)

	if engine.Database != db {
		t.Fatal("database was not attached")
	}
}

func TestSetDatabaseNilEngine(t *testing.T) {
	var engine *Engine

	engine.SetDatabase(nil)
}

func TestRunOncePersistsConfigurations(t *testing.T) {
	t.Skip("requires injectable collector/tester fixtures")
}

func TestRunOncePropagatesDatabaseSaveError(t *testing.T) {
	t.Skip("requires injectable database failure fixture")
}

func TestRunOncePersistsRuntimeState(t *testing.T) {
	t.Skip("requires deterministic probe fixture")
}

func TestRunOnceContextCancellation(t *testing.T) {
	engine := New(
		source.NewCollector(),
		tester.New(nil),
		nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := engine.RunOnce(ctx, nil)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
