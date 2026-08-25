package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/database"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/tester"
)

func TestRunOnceWithoutDatabase(t *testing.T) {
	engine := New(nil, nil, nil)

	result, err := engine.RunOnce(context.Background(), nil)
	if err == nil {
		t.Fatal("expected tester configuration error")
	}

	if result.FinishedAt.IsZero() {
		t.Fatal("expected FinishedAt to be set")
	}

	if result.Duration < 0 {
		t.Fatal("expected non-negative duration")
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
