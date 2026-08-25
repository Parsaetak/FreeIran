package archive

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
	ErrNilConfig   = errors.New("archive: nil configuration")
	ErrNotFound    = errors.New("archive: configuration not found")
	ErrInvalidPath = errors.New("archive: invalid path")
)

const (
	defaultFileMode os.FileMode = 0o600
	defaultDirMode  os.FileMode = 0o700
)

type Entry struct {
	Config    *config.Config `json:"config"`
	Archived  time.Time      `json:"archived"`
	LastError string         `json:"last_error,omitempty"`
}

type diskState struct {
	Version int               `json:"version"`
	Entries map[string]*Entry `json:"entries"`
}

type Archive struct {
	mu      sync.RWMutex
	path    string
	entries map[string]*Entry
}

func New(path string) (*Archive, error) {
	path = filepath.Clean(path)

	if path == "." || path == "" {
		return nil, ErrInvalidPath
	}

	return &Archive{
		path:    path,
		entries: make(map[string]*Entry),
	}, nil
}

func (a *Archive) Path() string {
	if a == nil {
		return ""
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.path
}

func (a *Archive) Count() int {
	if a == nil {
		return 0
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	return len(a.entries)
}

func (a *Archive) Has(id string) bool {
	if a == nil {
		return false
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	_, ok := a.entries[id]
	return ok
}

func (a *Archive) Get(id string) (*config.Config, error) {
	if a == nil {
		return nil, ErrNotFound
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	entry, ok := a.entries[id]
	if !ok || entry == nil || entry.Config == nil {
		return nil, ErrNotFound
	}

	return cloneConfig(entry.Config), nil
}

func (a *Archive) Add(cfg *config.Config, reason string) error {
	if a == nil {
		return ErrInvalidPath
	}

	if cfg == nil {
		return ErrNilConfig
	}

	normalized := *cfg
	normalized.Normalize()

	if err := normalized.Validate(); err != nil {
		return fmt.Errorf("archive: invalid configuration: %w", err)
	}

	if normalized.ID == "" {
		normalized.SetID()
	}

	now := time.Now().UTC()

	a.mu.Lock()
	defer a.mu.Unlock()

	a.entries[normalized.ID] = &Entry{
		Config:    cloneConfig(&normalized),
		Archived:  now,
		LastError: reason,
	}

	return nil
}

func (a *Archive) Remove(id string) error {
	if a == nil {
		return ErrNotFound
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.entries[id]; !ok {
		return ErrNotFound
	}

	delete(a.entries, id)

	return nil
}

func (a *Archive) List() []*config.Config {
	if a == nil {
		return nil
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	ids := make([]string, 0, len(a.entries))

	for id := range a.entries {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	result := make([]*config.Config, 0, len(ids))

	for _, id := range ids {
		entry := a.entries[id]

		if entry == nil || entry.Config == nil {
			continue
		}

		result = append(result, cloneConfig(entry.Config))
	}

	return result
}

func (a *Archive) Clear() {
	if a == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.entries = make(map[string]*Entry)
}

func (a *Archive) Load() error {
	if a == nil {
		return ErrInvalidPath
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	data, err := os.ReadFile(a.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.entries = make(map[string]*Entry)
			return nil
		}

		return fmt.Errorf("archive: read: %w", err)
	}

	var state diskState

	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("archive: decode: %w", err)
	}

	if state.Version != 1 {
		return fmt.Errorf("archive: unsupported version %d", state.Version)
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
			return fmt.Errorf(
				"archive: invalid entry %q: %w",
				id,
				err,
			)
		}

		loaded[cfg.ID] = &Entry{
			Config:    cloneConfig(&cfg),
			Archived:  entry.Archived,
			LastError: entry.LastError,
		}
	}

	a.entries = loaded

	return nil
}

func (a *Archive) Save() error {
	if a == nil {
		return ErrInvalidPath
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	state := diskState{
		Version: 1,
		Entries: make(map[string]*Entry, len(a.entries)),
	}

	for id, entry := range a.entries {
		if entry == nil || entry.Config == nil {
			continue
		}

		state.Entries[id] = &Entry{
			Config:    cloneConfig(entry.Config),
			Archived:  entry.Archived,
			LastError: entry.LastError,
		}
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("archive: encode: %w", err)
	}

	dir := filepath.Dir(a.path)

	if err := os.MkdirAll(dir, defaultDirMode); err != nil {
		return fmt.Errorf("archive: create directory: %w", err)
	}

	temp, err := os.CreateTemp(dir, ".freeiran-archive-*")
	if err != nil {
		return fmt.Errorf(
			"archive: create temporary file: %w",
			err,
		)
	}

	tempPath := temp.Name()

	defer func() {
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(defaultFileMode); err != nil {
		_ = temp.Close()

		return fmt.Errorf(
			"archive: set temporary file permissions: %w",
			err,
		)
	}

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()

		return fmt.Errorf("archive: write: %w", err)
	}

	if err := temp.Sync(); err != nil {
		_ = temp.Close()

		return fmt.Errorf("archive: sync: %w", err)
	}

	if err := temp.Close(); err != nil {
		return fmt.Errorf("archive: close: %w", err)
	}

	if err := os.Rename(tempPath, a.path); err != nil {
		return fmt.Errorf("archive: replace: %w", err)
	}

	// Explicitly enforce the intended private mode on the final path.
	//
	// On Unix-like systems this guarantees 0600 even if the destination
	// previously existed with broader permissions. On Windows, the Go
	// runtime does not expose Unix permission bits through Mode().Perm(),
	// so platform-specific tests must not require a literal 0600 there.
	if err := os.Chmod(a.path, defaultFileMode); err != nil {
		return fmt.Errorf(
			"archive: set final file permissions: %w",
			err,
		)
	}

	return nil
}

func cloneConfig(cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}

	copy := *cfg

	if cfg.AllowedIPs != nil {
		copy.AllowedIPs = append(
			[]string(nil),
			cfg.AllowedIPs...,
		)
	}

	if cfg.DNS != nil {
		copy.DNS = append(
			[]string(nil),
			cfg.DNS...,
		)
	}

	return &copy
}
