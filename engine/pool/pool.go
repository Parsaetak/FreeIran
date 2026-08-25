package pool

import (
	"fmt"
	"sort"
	"sync"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// Pool maintains the current configuration state in memory.
//
// Configuration identity is determined by the configuration fingerprint.
// The pool is concurrency-safe and keeps deterministic ordering.
type Pool struct {
	mu            sync.RWMutex
	configs       map[string]config.Config
	working       map[string]bool
	tested        map[string]bool
	lastErrors    map[string]string
}

// New creates an empty configuration pool.
func New() *Pool {
	return &Pool{
		configs:    make(map[string]config.Config),
		working:    make(map[string]bool),
		tested:     make(map[string]bool),
		lastErrors: make(map[string]string),
	}
}

// Add inserts a configuration.
//
// An existing fingerprint is rejected.
func (p *Pool) Add(cfg *config.Config) error {
	if p == nil {
		return fmt.Errorf("pool is nil")
	}

	if cfg == nil {
		return fmt.Errorf("configuration is nil")
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	cfg.Normalize()
	cfg.SetID()

	fingerprint := cfg.Fingerprint()

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.configs[fingerprint]; exists {
		return fmt.Errorf("configuration already exists: %s", fingerprint)
	}

	p.configs[fingerprint] = *cfg

	return nil
}

// Upsert inserts or replaces a configuration.
func (p *Pool) Upsert(cfg *config.Config) error {
	if p == nil {
		return fmt.Errorf("pool is nil")
	}

	if cfg == nil {
		return fmt.Errorf("configuration is nil")
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	cfg.Normalize()
	cfg.SetID()

	fingerprint := cfg.Fingerprint()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.configs[fingerprint] = *cfg

	return nil
}

// Get returns a configuration by fingerprint.
func (p *Pool) Get(fingerprint string) (*config.Config, bool) {
	if p == nil {
		return nil, false
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	cfg, exists := p.configs[fingerprint]
	if !exists {
		return nil, false
	}

	copy := cfg
	return &copy, true
}

// Remove deletes a configuration by fingerprint.
func (p *Pool) Remove(fingerprint string) bool {
	if p == nil {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.configs[fingerprint]; !exists {
		return false
	}

	delete(p.configs, fingerprint)
	delete(p.working, fingerprint)
	delete(p.tested, fingerprint)
	delete(p.lastErrors, fingerprint)

	return true
}

// Has reports whether a fingerprint exists.
func (p *Pool) Has(fingerprint string) bool {
	if p == nil {
		return false
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	_, exists := p.configs[fingerprint]
	return exists
}

// MarkTested records that a configuration has been tested.
func (p *Pool) MarkTested(fingerprint string, working bool, err error) error {
	if p == nil {
		return fmt.Errorf("pool is nil")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.configs[fingerprint]; !exists {
		return fmt.Errorf("configuration not found: %s", fingerprint)
	}

	p.tested[fingerprint] = true
	p.working[fingerprint] = working

	if err != nil {
		p.lastErrors[fingerprint] = err.Error()
	} else {
		delete(p.lastErrors, fingerprint)
	}

	return nil
}

// IsTested reports whether a configuration has been tested.
func (p *Pool) IsTested(fingerprint string) bool {
	if p == nil {
		return false
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.tested[fingerprint]
}

// IsWorking reports whether a tested configuration is working.
func (p *Pool) IsWorking(fingerprint string) bool {
	if p == nil {
		return false
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.working[fingerprint]
}

// LastError returns the latest testing error.
func (p *Pool) LastError(fingerprint string) string {
	if p == nil {
		return ""
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.lastErrors[fingerprint]
}

// List returns all configurations in deterministic fingerprint order.
func (p *Pool) List() []config.Config {
	if p == nil {
		return nil
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]config.Config, 0, len(p.configs))

	for _, cfg := range p.configs {
		result = append(result, cfg)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Fingerprint() < result[j].Fingerprint()
	})

	return result
}

// Working returns all known working configurations.
func (p *Pool) Working() []config.Config {
	return p.filter(func(fingerprint string) bool {
		return p.working[fingerprint]
	})
}

// Tested returns all configurations that have been tested.
func (p *Pool) Tested() []config.Config {
	return p.filter(func(fingerprint string) bool {
		return p.tested[fingerprint]
	})
}

// Failed returns all tested configurations that are not working.
func (p *Pool) Failed() []config.Config {
	return p.filter(func(fingerprint string) bool {
		return p.tested[fingerprint] && !p.working[fingerprint]
	})
}

// Count returns the total number of configurations.
func (p *Pool) Count() int {
	if p == nil {
		return 0
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	return len(p.configs)
}

// WorkingCount returns the number of working configurations.
func (p *Pool) WorkingCount() int {
	if p == nil {
		return 0
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0

	for fingerprint := range p.configs {
		if p.working[fingerprint] {
			count++
		}
	}

	return count
}

// TestedCount returns the number of tested configurations.
func (p *Pool) TestedCount() int {
	if p == nil {
		return 0
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0

	for fingerprint := range p.configs {
		if p.tested[fingerprint] {
			count++
		}
	}

	return count
}

func (p *Pool) filter(predicate func(string) bool) []config.Config {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]config.Config, 0)

	for fingerprint, cfg := range p.configs {
		if predicate(fingerprint) {
			result = append(result, cfg)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Fingerprint() < result[j].Fingerprint()
	})

	return result
}
