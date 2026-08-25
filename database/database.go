package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

var (
	ErrNilConfig       = errors.New("database: nil configuration")
	ErrNotFound        = errors.New("database: configuration not found")
	ErrInvalidPath     = errors.New("database: invalid path")
	ErrDuplicateConfig = errors.New("database: duplicate configuration")
)

const (
	defaultFileMode os.FileMode = 0o600
	defaultDirMode  os.FileMode = 0o700
)

type Entry struct {
	Config *config.Config `json:"config"`
	Added  time.Time     `json:"added"`
	Updated time.Time    `json:"updated"`
}

type diskState struct {
	Version int                `json:"version"`
	Entries map[string]*Entry  `json:"entries"`
}

type Database struct {
	mu      sync.RWMutex
	path    string
	entries map[string]*Entry
}

func New(path string) (*Database, error) {
	path = filepath.Clean(path)

	if path == "." || path == "" {
		return nil, ErrInvalidPath
	}

	return &Database{
		path:    path,
		entries: make(map[string]*Entry),
	}, nil
}

func (db *Database) Path() string {
	db.mu.RLock()
	defer db.mu.RUnlock()

	return db.path
}

func (db *Database) Count() int {
	db.mu.RLock()
	defer db.mu.RUnlock()

	return len(db.entries)
}

func (db *Database) Get(id string) (*config.Config, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	entry, ok := db.entries[id]
	if !ok || entry == nil || entry.Config == nil {
		return nil, ErrNotFound
	}

	return cloneConfig(entry.Config), nil
}

func (db *Database) Has(id string) bool {
	db.mu.RLock()
	defer db.mu.RUnlock()

	_, ok := db.entries[id]
	return ok
}

func (db *Database) Upsert(cfg *config.Config) error {
	if cfg == nil {
		return ErrNilConfig
	}

	normalized := *cfg
	normalized.Normalize()

	if err := normalized.Validate(); err != nil {
		return fmt.Errorf("database: invalid configuration: %w", err)
	}

	if normalized.ID == "" {
		normalized.SetID()
	}

	now := time.Now().UTC()

	db.mu.Lock()
	defer db.mu.Unlock()

	if existing, ok := db.entries[normalized.ID]; ok && existing != nil {
		existing.Config = cloneConfig(&normalized)
		existing.Updated = now
		return nil
	}

	db.entries[normalized.ID] = &Entry{
		Config:  cloneConfig(&normalized),
		Added:   now,
		Updated: now,
	}

	return nil
}

func (db *Database) Add(cfg *config.Config) error {
	if cfg == nil {
		return ErrNilConfig
	}

	normalized := *cfg
	normalized.Normalize()

	if err := normalized.Validate(); err != nil {
		return fmt.Errorf("database: invalid configuration: %w", err)
	}

	if normalized.ID == "" {
		normalized.SetID()
	}

	now := time.Now().UTC()

	db.mu.Lock()
	defer db.mu.Unlock()

	if _, exists := db.entries[normalized.ID]; exists {
		return ErrDuplicateConfig
	}

	db.entries[normalized.ID] = &Entry{
		Config:  cloneConfig(&normalized),
		Added:   now,
		Updated: now,
	}

	return nil
}

func (db *Database) Remove(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if _, ok := db.entries[id]; !ok {
		return ErrNotFound
	}

	delete(db.entries, id)
	return nil
}

func (db *Database) List() []*config.Config {
	db.mu.RLock()
	defer db.mu.RUnlock()

	ids := make([]string, 0, len(db.entries))

	for id := range db.entries {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	result := make([]*config.Config, 0, len(ids))

	for _, id := range ids {
		entry := db.entries[id]
		if entry == nil || entry.Config == nil {
			continue
		}

		result = append(result, cloneConfig(entry.Config))
	}

	return result
}

func (db *Database) Clear() {
	db.mu.Lock()
	defer db.mu.Unlock()

	db.entries = make(map[string]*Entry)
}

func (db *Database) Load() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	data, err := os.ReadFile(db.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			db.entries = make(map[string]*Entry)
			return nil
		}

		return fmt.Errorf("database: read: %w", err)
	}

	var state diskState

	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("database: decode: %w", err)
	}

	if state.Version != 1 {
		return fmt.Errorf("database: unsupported version %d", state.Version)
	}

	if state.Entries == nil {
		state.Entries = make(map[string]*Entry)
	}

	loaded := make(map[string]*Entry, len(state.Entries))

	for id, entry := range state.Entries {
		if entry == nil || entry.Config == nil {
			continue
		}

		cfg := *entry.Config
		cfg.Normalize()

		if cfg.ID == "" {
			cfg.SetID()
		}

		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("database: invalid entry %q: %w", id, err)
		}

		loaded[cfg.ID] = &Entry{
			Config:  cloneConfig(&cfg),
			Added:   entry.Added,
			Updated: entry.Updated,
		}
	}

	db.entries = loaded
	return nil
}

func (db *Database) Save() error {
	db.mu.RLock()
	defer db.mu.RUnlock()

	state := diskState{
		Version: 1,
		Entries: make(map[string]*Entry, len(db.entries)),
	}

	for id, entry := range db.entries {
		if entry == nil || entry.Config == nil {
			continue
		}

		state.Entries[id] = &Entry{
			Config:  cloneConfig(entry.Config),
			Added:   entry.Added,
			Updated: entry.Updated,
		}
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("database: encode: %w", err)
	}

	dir := filepath.Dir(db.path)

	if err := os.MkdirAll(dir, defaultDirMode); err != nil {
		return fmt.Errorf("database: create directory: %w", err)
	}

	temp, err := os.CreateTemp(dir, ".freeiran-db-*")
	if err != nil {
		return fmt.Errorf("database: create temporary file: %w", err)
	}

	tempPath := temp.Name()

	defer func() {
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(defaultFileMode); err != nil {
		_ = temp.Close()
		return fmt.Errorf("database: set file permissions: %w", err)
	}

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("database: write: %w", err)
	}

	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("database: sync: %w", err)
	}

	if err := temp.Close(); err != nil {
		return fmt.Errorf("database: close: %w", err)
	}

	if err := os.Rename(tempPath, db.path); err != nil {
		return fmt.Errorf("database: replace: %w", err)
	}

	return nil
}

func cloneConfig(cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}

	copy := *cfg
	return &copy
}
