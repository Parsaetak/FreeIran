package source

import (
	"context"
	"fmt"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/parser"
)

// Collector downloads and parses configuration sources.
type Collector struct {
	Fetcher *Fetcher
	Parser  *parser.Parser
}

// NewCollector creates a Collector with the standard engine components.
func NewCollector() *Collector {
	return &Collector{
		Fetcher: NewFetcher(),
		Parser:  parser.New(),
	}
}

// Collection contains the configurations discovered from one source.
type Collection struct {
	Source        Source
	Configurations []config.Config
}

// Collect downloads and parses one source.
func (c *Collector) Collect(
	ctx context.Context,
	src Source,
) (Collection, error) {
	if strings.TrimSpace(src.ID) == "" {
		return Collection{}, fmt.Errorf("source ID is empty")
	}

	if strings.TrimSpace(src.URL) == "" {
		return Collection{}, fmt.Errorf("source URL is empty")
	}

	if !src.Enabled {
		return Collection{}, fmt.Errorf("source %q is disabled", src.ID)
	}

	fetcher := c.Fetcher
	if fetcher == nil {
		fetcher = NewFetcher()
	}

	p := c.Parser
	if p == nil {
		p = parser.New()
	}

	result, err := fetcher.Fetch(ctx, src)
	if err != nil {
		return Collection{}, err
	}

	configurations, err := p.Parse(result.Content)
	if err != nil {
		return Collection{}, fmt.Errorf(
			"parse source %q: %w",
			src.ID,
			err,
		)
	}

	return Collection{
		Source:         src,
		Configurations: configurations,
	}, nil
}

// CollectAll downloads and parses all enabled sources.
//
// A failure in one source does not discard successful results from
// other sources. The returned error contains the combined failures.
func (c *Collector) CollectAll(
	ctx context.Context,
	sources []Source,
) ([]Collection, error) {
	if len(sources) == 0 {
		return nil, nil
	}

	collections := make([]Collection, 0, len(sources))
	var errors []string

	for _, src := range sources {
		collection, err := c.Collect(ctx, src)
		if err != nil {
			errors = append(
				errors,
				fmt.Sprintf("%s: %v", src.ID, err),
			)
			continue
		}

		collections = append(collections, collection)
	}

	if len(errors) > 0 {
		return collections, fmt.Errorf(
			"one or more sources failed: %s",
			strings.Join(errors, "; "),
		)
	}

	return collections, nil
}

// MergeConfigurations combines configurations from multiple collections
// and removes duplicates using their canonical fingerprints.
func MergeConfigurations(
	collections []Collection,
) []config.Config {
	if len(collections) == 0 {
		return nil
	}

	unique := make(map[string]config.Config)

	for _, collection := range collections {
		for _, cfg := range collection.Configurations {
			cfg.Normalize()

			if err := cfg.Validate(); err != nil {
				continue
			}

			fingerprint := cfg.Fingerprint()

			if fingerprint == "" {
				continue
			}

			if _, exists := unique[fingerprint]; exists {
				continue
			}

			unique[fingerprint] = cfg
		}
	}

	result := make([]config.Config, 0, len(unique))

	for _, cfg := range unique {
		result = append(result, cfg)
	}

	return result
}
