package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// Core is the execution boundary for one protocol family.
//
// A Core implementation is responsible for translating a normalized Config
// into a running protocol instance and testing whether that instance works.
type Core interface {
	Type() config.Type

	Start(context.Context, config.Config) (Instance, error)
}

// Instance represents one running protocol instance.
type Instance interface {
	Endpoint() string
	Close() error
}

// Registry maps configuration protocols to their execution cores.
type Registry struct {
	mu    sync.RWMutex
	cores map[config.Type]Core
}

// NewRegistry creates an empty core registry.
func NewRegistry() *Registry {
	return &Registry{
		cores: make(map[config.Type]Core),
	}
}

// Register adds or replaces a protocol core.
func (r *Registry) Register(c Core) error {
	if c == nil {
		return fmt.Errorf("core is nil")
	}

	protocol := c.Type()

	if protocol == config.TypeUnknown {
		return fmt.Errorf("core protocol is unknown")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.cores[protocol] = c

	return nil
}

// Get returns the core responsible for a protocol.
func (r *Registry) Get(protocol config.Type) (Core, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	c, ok := r.cores[protocol]

	return c, ok
}

// Supports reports whether a protocol has a registered execution core.
func (r *Registry) Supports(protocol config.Type) bool {
	_, ok := r.Get(protocol)
	return ok
}

// Types returns all registered protocol types.
func (r *Registry) Types() []config.Type {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]config.Type, 0, len(r.cores))

	for protocol := range r.cores {
		result = append(result, protocol)
	}

	return result
}

// Start resolves the appropriate core and starts a configuration.
func (r *Registry) Start(
	ctx context.Context,
	cfg config.Config,
) (Instance, error) {
	if r == nil {
		return nil, fmt.Errorf("core registry is nil")
	}

	cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	c, ok := r.Get(cfg.Type)
	if !ok {
		return nil, fmt.Errorf(
			"no execution core registered for protocol %q",
			cfg.Type,
		)
	}

	instance, err := c.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf(
			"start %s core: %w",
			cfg.Type,
			err,
		)
	}

	if instance == nil {
		return nil, fmt.Errorf(
			"core %q returned a nil instance",
			cfg.Type,
		)
	}

	return instance, nil
}
