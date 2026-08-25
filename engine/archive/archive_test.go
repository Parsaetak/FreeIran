package archive

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func testConfig(name, address string, port int) *config.Config {
	cfg := &config.Config{
		Type:    config.TypeHTTP,
		Name:    name,
		Address: address,
		Port:    port,
	}

	cfg.Normalize()
	cfg.SetID()

	return cfg
}

func TestNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.json")

	a, err := New(path)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	if a.Path() != filepath.Clean(path) {
		t.Fatalf("unexpected path: %q", a.Path())
	}

	if a.Count() != 0 {
		t.Fatalf("expected empty archive, got %d entries", a.Count())
	}
}

func TestNewRejectsInvalidPath(t *testing.T) {
	for _, path := range []string{"", "."} {
		t.Run(path, func(t *testing.T) {
			if _, err := New(path); !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("expected ErrInvalidPath, got %v", err)
			}
		})
	}
}

func TestAddAndGet(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig("Example", "Example.COM", 443)
	originalID := cfg.ID

	if err := a.Add(cfg, "connection failed"); err != nil {
		t.Fatalf("Add() failed: %v", err)
	}

	if a.Count() != 1 {
		t.Fatalf("expected one entry, got %d", a.Count())
	}

	got, err := a.Get(originalID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}

	if got.ID != originalID || got.Address != "example.com" || got.Port != 443 {
		t.Fatalf("unexpected configuration: %#v", got)
	}

	got.Name = "mutated"

	stored, err := a.Get(originalID)
	if err != nil {
		t.Fatal(err)
	}

	if stored.Name == "mutated" {
		t.Fatal("Get() returned mutable internal state")
	}
}

func TestAddReplacesExistingConfiguration(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig("Example", "example.com", 443)

	if err := a.Add(cfg, "first failure"); err != nil {
		t.Fatal(err)
	}

	if err := a.Add(cfg, "second failure"); err != nil {
		t.Fatal(err)
	}

	if a.Count() != 1 {
		t.Fatalf("expected one entry, got %d", a.Count())
	}

	if !a.Has(cfg.ID) {
		t.Fatal("expected configuration to exist")
	}
}

func TestAddRejectsNilConfiguration(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Add(nil, "failure"); !errors.Is(err, ErrNilConfig) {
		t.Fatalf("expected ErrNilConfig, got %v", err)
	}
}

func TestAddRejectsInvalidConfiguration(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Type:    config.TypeVLESS,
		Address: "example.com",
		Port:    443,
	}

	if err := a.Add(cfg, "failure"); err == nil {
		t.Fatal("expected invalid configuration error")
	}
}

func TestHas(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig("Example", "example.com", 443)

	if a.Has(cfg.ID) {
		t.Fatal("configuration should not exist before Add")
	}

	if err := a.Add(cfg, "failure"); err != nil {
		t.Fatal(err)
	}

	if !a.Has(cfg.ID) {
		t.Fatal("configuration should exist after Add")
	}
}

func TestGetMissing(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRemove(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig("Example", "example.com", 443)

	if err := a.Add(cfg, "failure"); err != nil {
		t.Fatal(err)
	}

	if err := a.Remove(cfg.ID); err != nil {
		t.Fatalf("Remove() failed: %v", err)
	}

	if a.Has(cfg.ID) {
		t.Fatal("configuration still exists after Remove")
	}
}

func TestRemoveMissing(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Remove("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListIsDeterministic(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	configs := []*config.Config{
		testConfig("C", "c.example", 443),
		testConfig("A", "a.example", 443),
		testConfig("B", "b.example", 443),
	}

	for _, cfg := range configs {
		if err := a.Add(cfg, "failure"); err != nil {
			t.Fatal(err)
		}
	}

	first := a.List()
	second := a.List()

	if !reflect.DeepEqual(first, second) {
		t.Fatal("List() is not deterministic")
	}

	for i := 1; i < len(first); i++ {
		if first[i-1].ID > first[i].ID {
			t.Fatal("List() is not sorted by ID")
		}
	}
}

func TestClear(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "archive.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Add(testConfig("Example", "example.com", 443), "failure"); err != nil {
		t.Fatal(err)
	}

	a.Clear()

	if a.Count() != 0 {
		t.Fatalf("expected empty archive, got %d entries", a.Count())
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "archive.json")

	a, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig("Example", "example.com", 443)

	if err := a.Add(cfg, "connection refused"); err != nil {
		t.Fatal(err)
	}

	if err := a.Save(); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	b, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := b.Load(); err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	got, err := b.Get(cfg.ID)
	if err != nil {
		t.Fatalf("Get() after Load failed: %v", err)
	}

	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf(
			"loaded configuration differs:\nwant: %#v\n got: %#v",
			cfg,
			got,
		)
	}
}

func TestSavePersistsArchiveMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.json")

	a, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig("Example", "example.com", 443)
	before := time.Now().UTC()

	if err := a.Add(cfg, "timeout"); err != nil {
		t.Fatal(err)
	}

	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var state struct {
		Version int `json:"version"`
		Entries map[string]struct {
			Archived  time.Time `json:"archived"`
			LastError string    `json:"last_error"`
		} `json:"entries"`
	}

	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("invalid saved JSON: %v", err)
	}

	if state.Version != 1 {
		t.Fatalf("expected archive version 1, got %d", state.Version)
	}

	entry, ok := state.Entries[cfg.ID]
	if !ok {
		t.Fatal("saved archive entry is missing")
	}

	if entry.LastError != "timeout" {
		t.Fatalf("expected persisted reason, got %q", entry.LastError)
	}

	now := time.Now().UTC()

	if entry.Archived.Before(before) || entry.Archived.After(now) {
		t.Fatalf("archive timestamp is outside expected range: %v", entry.Archived)
	}
}

func TestLoadMissingFileCreatesEmptyArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "archive.json")

	a, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Load(); err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if a.Count() != 0 {
		t.Fatalf("expected empty archive, got %d entries", a.Count())
	}
}

func TestLoadRejectsCorruptArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.json")

	if err := os.WriteFile(
		path,
		[]byte("not valid json"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	a, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Load(); err == nil {
		t.Fatal("expected corrupt archive error")
	}
}

func TestLoadRejectsUnsupportedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.json")

	data := []byte(`{"version":99,"entries":{}}`)

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Load(); err == nil {
		t.Fatal("expected unsupported version error")
	}
}

func TestSaveCreatesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.json")

	a, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	switch runtime.GOOS {
	case "windows", "android":
		// Unix permission bits are not reliable on these platforms.
		return
	default:
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("archive file is not private: %o", mode)
		}
	}
}

func TestNilArchive(t *testing.T) {
	var a *Archive

	if a.Path() != "" {
		t.Fatal("nil archive Path() should return empty string")
	}

	if a.Count() != 0 {
		t.Fatal("nil archive Count() should return zero")
	}

	if a.Has("id") {
		t.Fatal("nil archive Has() should return false")
	}

	if _, err := a.Get("id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound from nil Get(), got %v", err)
	}

	if err := a.Add(
		testConfig("Example", "example.com", 443),
		"failure",
	); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("expected ErrInvalidPath from nil Add(), got %v", err)
	}

	if err := a.Remove("id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound from nil Remove(), got %v", err)
	}

	if err := a.Load(); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("expected ErrInvalidPath from nil Load(), got %v", err)
	}

	if err := a.Save(); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("expected ErrInvalidPath from nil Save(), got %v", err)
	}

	a.Clear()
}
