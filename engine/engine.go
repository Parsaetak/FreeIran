package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/database"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/tester"
)

// Engine coordinates one complete configuration-testing cycle.
//
// The engine deliberately contains no platform-specific code.
// Windows and Android can both use the same orchestration layer.
type Engine struct {
	Collector *source.Collector
	Tester    *tester.Tester
	Registry  *core.Registry
	Database  *database.Database

	mu sync.RWMutex
}

// CycleResult contains the complete result of one engine cycle.
type CycleResult struct {
	StartedAt   time.Time
	FinishedAt  time.Time
	Duration    time.Duration
	Discovered  int
	Unique      int
	Tested      int
	Working     int
	Failed      int
	Configurations []config.Config
}

// New creates an Engine using the supplied components.
//
// Nil components are replaced with safe defaults where possible.
func New(
	collector *source.Collector,
	testerInstance *tester.Tester,
	registry *core.Registry,
) *Engine {
	if collector == nil {
		collector = source.NewCollector()
	}

	return &Engine{
		Collector: collector,
		Tester:    testerInstance,
		Registry:  registry,
	}
}

// SetDatabase attaches a persistent configuration database to the engine.
func (e *Engine) SetDatabase(db *database.Database) {
	if e == nil {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.Database = db
}
// RunOnce executes one complete discovery and testing cycle.
//
// The cycle:
//  1. collects enabled sources,
//  2. merges and deduplicates configurations,
//  3. tests every configuration,
//  4. keeps the resulting runtime state.
//
// Source failures do not prevent successful sources from being processed.
func (e *Engine) RunOnce(
	ctx context.Context,
	sources []source.Source,
) (CycleResult, error) {
	started := time.Now().UTC()

	result := CycleResult{
		StartedAt: started,
	}

	if e == nil {
		return result, fmt.Errorf("engine is nil")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	e.mu.RLock()
collector := e.Collector
testerInstance := e.Tester
registry := e.Registry
db := e.Database
e.mu.RUnlock()

	if collector == nil {
		collector = source.NewCollector()
	}

	collections, collectErr := collector.CollectAll(ctx, sources)

	configurations := source.MergeConfigurations(collections)

	result.Discovered = countConfigurations(collections)
	result.Unique = len(configurations)

	// If there is no tester, discovery still works, but configurations
	// cannot be classified as working.
	if testerInstance == nil {
		result.FinishedAt = time.Now().UTC()
		result.Duration = result.FinishedAt.Sub(started)

		if collectErr != nil {
			return result, collectErr
		}

		return result, fmt.Errorf("engine tester is not configured")
	}

	// Test configurations independently. A single failed configuration
	// must never stop the remainder of the cycle.
	for i := range configurations {
		if err := ctx.Err(); err != nil {
			result.FinishedAt = time.Now().UTC()
			result.Duration = result.FinishedAt.Sub(started)

			return result, err
		}

		cfg := &configurations[i]

		// If a registry exists, do not send configurations to a tester
		// that cannot execute their protocol.
		if registry != nil && !registry.Supports(cfg.Type) {
	cfg.Working = false
	cfg.TestedAt = time.Now().UTC().UnixMilli()

	result.Tested++
	result.Failed++

	continue
}

		testResult := testerInstance.TestAndApply(ctx, cfg)

		result.Tested++

		if testResult.Working {
			result.Working++
		} else {
			result.Failed++
		}
	}

	// Fingerprints provide deterministic identity. Sorting makes the cycle
	// result stable regardless of Go map iteration order in the collector.
	sort.Slice(configurations, func(i, j int) bool {
		return configurations[i].Fingerprint() <
			configurations[j].Fingerprint()
	})

	result.Configurations = configurations
	if db != nil {
	for i := range configurations {
		if err := db.Upsert(&configurations[i]); err != nil {
			result.FinishedAt = time.Now().UTC()
			result.Duration = result.FinishedAt.Sub(started)

			return result, fmt.Errorf(
				"persist configuration %s: %w",
				configurations[i].ID,
				err,
			)
		}
	}

	if err := db.Save(); err != nil {
		result.FinishedAt = time.Now().UTC()
		result.Duration = result.FinishedAt.Sub(started)

		return result, fmt.Errorf("persist database: %w", err)
	}
}
	result.FinishedAt = time.Now().UTC()
	result.Duration = result.FinishedAt.Sub(started)

	if collectErr != nil {
		return result, collectErr
	}

	return result, nil
}

// countConfigurations counts all configurations discovered before
// deduplication.
func countConfigurations(collections []source.Collection) int {
	total := 0

	for _, collection := range collections {
		total += len(collection.Configurations)
	}

	return total
}

// NewWithDatabase creates an Engine and loads its persistent database.
func NewWithDatabase(
	collector *source.Collector,
	testerInstance *tester.Tester,
	registry *core.Registry,
	path string,
) (*Engine, error) {
	db, err := database.New(path)
	if err != nil {
		return nil, err
	}

	if err := db.Load(); err != nil {
		return nil, err
	}

	engine := New(collector, testerInstance, registry)
	engine.SetDatabase(db)

	return engine, nil
}


