package database

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func testConfig() *config.Config {
	cfg := &config.Config{
		Type:    config.TypeVLESS,
		Address: "example.com",
		Port:    443,
		UUID:    "11111111-1111-1111-1111-111111111111",
		Network: "tcp",
	}

	cfg.Normalize()
	cfg.SetID()

	return cfg
}

func TestNewDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")

	db, err := New(path)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	if db.Path() != path {
		t.Fatalf("unexpected path: %q", db.Path())
	}

	if db.Count() != 0 {
		t.Fatalf("new database should be empty")
	}
}

func TestNewRejectsInvalidPath(t *testing.T) {
	db, err := New("")
	if !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("expected ErrInvalidPath, got %v", err)
	}

	if db != nil {
		t.Fatalf("expected nil database")
	}
}

func TestAddAndGet(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()

	if err := db.Add(cfg); err != nil {
		t.Fatalf("Add() failed: %v", err)
	}

	if db.Count() != 1 {
		t.Fatalf("expected count 1, got %d", db.Count())
	}

	got, err := db.Get(cfg.ID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if got.ID != cfg.ID {
		t.Fatalf("ID mismatch: got %q want %q", got.ID, cfg.ID)
	}

	if got.Address != cfg.Address {
		t.Fatalf("address mismatch: got %q want %q", got.Address, cfg.Address)
	}
}

func TestAddRejectsDuplicate(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()

	if err := db.Add(cfg); err != nil {
		t.Fatal(err)
	}

	err = db.Add(cfg)

	if !errors.Is(err, ErrDuplicateConfig) {
		t.Fatalf("expected ErrDuplicateConfig, got %v", err)
	}

	if db.Count() != 1 {
		t.Fatalf("expected one entry, got %d", db.Count())
	}
}

func TestUpsertReplacesExistingConfiguration(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()

	if err := db.Add(cfg); err != nil {
		t.Fatal(err)
	}

	updated := *cfg
	updated.Address = "updated.example.com"
	updated.SetID()

	if err := db.Upsert(&updated); err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	if db.Count() != 1 {
		t.Fatalf("expected one entry, got %d", db.Count())
	}

	got, err := db.Get(updated.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.Address != "updated.example.com" {
		t.Fatalf("configuration was not updated")
	}
}

func TestRemove(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()

	if err := db.Add(cfg); err != nil {
		t.Fatal(err)
	}

	if err := db.Remove(cfg.ID); err != nil {
		t.Fatalf("Remove() failed: %v", err)
	}

	if db.Count() != 0 {
		t.Fatalf("expected empty database")
	}

	if _, err := db.Get(cfg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRemoveMissing(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}

	err = db.Remove("missing")

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestHas(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()

	if db.Has(cfg.ID) {
		t.Fatalf("configuration should not exist yet")
	}

	if err := db.Add(cfg); err != nil {
		t.Fatal(err)
	}

	if !db.Has(cfg.ID) {
		t.Fatalf("configuration should exist")
	}
}

func TestListIsDeterministic(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}

	first := testConfig()

	second := testConfig()
	second.Address = "another.example.com"
	second.SetID()

	if err := db.Add(second); err != nil {
		t.Fatal(err)
	}

	if err := db.Add(first); err != nil {
		t.Fatal(err)
	}

	list := db.List()

	if len(list) != 2 {
		t.Fatalf("expected two entries, got %d", len(list))
	}

	if list[0].ID > list[1].ID {
		t.Fatalf("List() is not sorted by ID")
	}
}

func TestClear(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Add(testConfig()); err != nil {
		t.Fatal(err)
	}

	db.Clear()

	if db.Count() != 0 {
		t.Fatalf("database should be empty after Clear()")
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")

	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()

	if err := db.Add(cfg); err != nil {
		t.Fatal(err)
	}

	if err := db.Save(); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	loaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := loaded.Load(); err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if loaded.Count() != 1 {
		t.Fatalf("expected one loaded entry, got %d", loaded.Count())
	}

	got, err := loaded.Get(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.ID != cfg.ID {
		t.Fatalf("ID mismatch after reload")
	}

	if got.Address != cfg.Address {
		t.Fatalf("address mismatch after reload")
	}
}

func TestLoadMissingFileCreatesEmptyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")

	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Load(); err != nil {
		t.Fatalf("Load() should accept missing file: %v", err)
	}

	if db.Count() != 0 {
		t.Fatalf("expected empty database")
	}
}

func TestLoadRejectsCorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.json")

	if err := os.WriteFile(path, []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Load(); err == nil {
		t.Fatalf("expected corrupt database to fail")
	}
}

func TestSaveCreatesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "database.json")

	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Add(testConfig()); err != nil {
		t.Fatal(err)
	}

	if err := db.Save(); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database file is not private: %o", info.Mode().Perm())
	}
}

func TestNilConfiguration(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "database.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Add(nil); !errors.Is(err, ErrNilConfig) {
		t.Fatalf("Add(nil): expected ErrNilConfig, got %v", err)
	}

	if err := db.Upsert(nil); !errors.Is(err, ErrNilConfig) {
		t.Fatalf("Upsert(nil): expected ErrNilConfig, got %v", err)
	}
}
