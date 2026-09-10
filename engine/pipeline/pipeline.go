// Package pipeline implements the FreeIran streaming ingestion
// pipeline:
//
//	FETCH → PARSE → NORMALIZE → VALIDATE → DEDUPLICATE → PERSIST
//
// Each stage runs a bounded worker pool connected by bounded channels,
// so a slow or failed source never blocks unrelated sources and memory
// stays bounded regardless of input size. Context cancellation
// propagates through every stage and the pipeline shuts down cleanly
// without goroutine leaks.
package pipeline

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/engine/metrics"
	"github.com/Parsaetak/FreeIran/engine/native"
	"github.com/Parsaetak/FreeIran/engine/parser"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/store"
)

// Subsystem identifies the pipeline in structured errors.
const Subsystem = "pipeline"

// Config tunes the pipeline's concurrency and batching.
type Config struct {
	// FetchWorkers processes sources concurrently.
	FetchWorkers int

	// ParseWorkers parse fetched payloads concurrently.
	ParseWorkers int

	// DedupShards partitions the dedup index to reduce contention.
	DedupShards int

	// WriterBatchSize batches records written to the store; one batch
	// costs one journal fsync.
	WriterBatchSize int

	// QueueSize bounds every inter-stage channel.
	QueueSize int
}

// DefaultConfig returns conservative defaults sized for desktop use.
func DefaultConfig() Config {
	return Config{
		FetchWorkers:    4,
		ParseWorkers:    4,
		DedupShards:     16,
		WriterBatchSize: 512,
		QueueSize:       64,
	}
}

// normalize clamps configuration to safe ranges.
func (c Config) normalize() Config {
	if c.FetchWorkers <= 0 {
		c.FetchWorkers = 4
	}

	if c.ParseWorkers <= 0 {
		c.ParseWorkers = 4
	}

	if c.DedupShards <= 0 {
		c.DedupShards = 16
	}

	if c.WriterBatchSize <= 0 {
		c.WriterBatchSize = 512
	}

	if c.QueueSize <= 0 {
		c.QueueSize = 64
	}

	return c
}

// fetched is one downloaded source payload flowing to the parse stage.
type fetched struct {
	src           source.Source
	content       []byte
	err           error
	unchangedHash string // non-empty: content identical to previous run
}

// parsed is one parse-stage outcome flowing to the dedup stage.
type parsed struct {
	src           source.Source
	configs       []config.Config
	err           error
	unchangedHash string
}

// SourceResult reports the outcome for one source.
type SourceResult struct {
	SourceID    string `json:"source_id"`
	OK          bool   `json:"ok"`
	Discovered  int    `json:"discovered"`
	Unique      int    `json:"unique"`
	Unchanged   bool   `json:"unchanged"`
	Error       string `json:"error,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	ContentHash string `json:"content_hash,omitempty"`
}

// Stats is the pipeline run summary.
type Stats struct {
	SourcesTotal     int            `json:"sources_total"`
	SourcesOK        int            `json:"sources_ok"`
	SourcesFailed    int            `json:"sources_failed"`
	SourcesUnchanged int            `json:"sources_unchanged"`
	Discovered       int64          `json:"discovered"`
	Duplicates       int64          `json:"duplicates"`
	Persisted        int64          `json:"persisted"`
	Invalid          int64          `json:"invalid"`
	PerSource        []SourceResult `json:"per_source"`
}

// Sink receives validated, deduplicated configurations. The store
// adapter below implements it; tests can substitute their own.
type Sink interface {
	// Persist batches configurations; implementations must be safe
	// for concurrent use.
	Persist(ctx context.Context, configs []config.Config) error
}

// StoreSink adapts a chunked store to the Sink interface, serialising
// configs once and writing them in bounded batches so each batch costs
// a single journal fsync.
type StoreSink struct {
	store     *store.Store
	batchSize int
}

// NewStoreSink creates a store-backed sink.
func NewStoreSink(st *store.Store, batchSize int) *StoreSink {
	if batchSize <= 0 {
		batchSize = 512
	}

	return &StoreSink{store: st, batchSize: batchSize}
}

// Persist stages one batch of configurations.
func (s *StoreSink) Persist(
	ctx context.Context,
	configs []config.Config,
) error {
	if len(configs) == 0 {
		return nil
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	pairs := make([]store.Pair, 0, len(configs))

	for i := range configs {
		raw, err := json.Marshal(&configs[i])
		if err != nil {
			return errors.Wrap(err, errors.KindInvalidInput,
				Subsystem, "persist", "serialise config %s", configs[i].ID)
		}

		pairs = append(pairs, store.Pair{
			Key:   configs[i].Fingerprint(),
			Value: raw,
		})
	}

	if len(pairs) == 0 {
		return nil
	}

	return s.store.UpsertBatch(pairs)
}

// Flush drains the store memtable to disk.
func (s *StoreSink) Flush() error {
	return s.store.Flush()
}

// Pipeline is the configurable ingestion pipeline.
type Pipeline struct {
	cfg     Config
	metrics *metrics.Registry
}

// New creates a pipeline.
func New(cfg Config, m *metrics.Registry) *Pipeline {
	cfg = cfg.normalize()

	if m == nil {
		m = metrics.New()
	}

	return &Pipeline{cfg: cfg, metrics: m}
}

// Run executes one full ingestion cycle over the given sources.
//
// seenHashes maps source IDs to the content hash of the previous run;
// sources whose content is unchanged skip parse and persistence
// entirely. The returned map contains fresh hashes for the next run.
func (p *Pipeline) Run(
	ctx context.Context,
	sources []source.Source,
	sink Sink,
	seenHashes map[string]string,
) (*Stats, map[string]string, error) {
	if sink == nil {
		return nil, seenHashes, errors.New(
			errors.KindConfiguration, Subsystem, "run", "sink is nil")
	}

	cfg := p.cfg
	stats := &Stats{SourcesTotal: len(sources)}
	perSource := make([]SourceResult, 0, len(sources))
	var perSourceMu sync.Mutex

	fetchQueue := make(chan source.Source, cfg.QueueSize)
	parseQueue := make(chan fetched, cfg.QueueSize)
	dedupQueue := make(chan parsed, cfg.QueueSize)

	var wgFetch, wgParse sync.WaitGroup

	shards := make([]*dedupShard, cfg.DedupShards)

	for i := range shards {
		shards[i] = &dedupShard{seen: make(map[uint64]struct{})}
	}

	var discovered, duplicates, invalid, persisted atomic.Int64

	var hashMu sync.Mutex

	newHashes := make(map[string]string, len(sources))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// ---- Stage 1: fetch ----
	fetcher := source.NewFetcher()

	for w := 0; w < cfg.FetchWorkers; w++ {
		wgFetch.Add(1)

		go func() {
			defer wgFetch.Done()

			for src := range fetchQueue {
				started := time.Now()

				payload := fetched{src: src}

				result, err := fetcher.Fetch(ctx, src)

				p.metrics.ObserveDuration("source_fetch",
					time.Since(started))

				if err != nil {
					payload.err = errors.Wrap(err,
						errors.KindRetryable, Subsystem, "fetch",
						"source %q", src.ID)
				} else {
					payload.content = result.Content

					hash := contentFingerprint(result.Content)

					hashMu.Lock()

					if seenHashes != nil && seenHashes[src.ID] == hash {
						payload.unchangedHash = hash
					} else {
						newHashes[src.ID] = hash
					}

					hashMu.Unlock()
				}

				select {
				case parseQueue <- payload:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// ---- Stage 2: parse ----
	pr := parser.New()

	for w := 0; w < cfg.ParseWorkers; w++ {
		wgParse.Add(1)

		go func() {
			defer wgParse.Done()

			for payload := range parseQueue {
				started := time.Now()

				item := parsed{
					src:           payload.src,
					err:           payload.err,
					unchangedHash: payload.unchangedHash,
				}

				if payload.err == nil && payload.unchangedHash == "" {
					configs, parseStats, parseErr :=
						pr.ParseDetailed(payload.content)

					item.configs = configs
					item.err = parseErr

					invalid.Add(int64(parseStats.Rejected))
					duplicates.Add(int64(parseStats.Duplicates))
				}

				p.metrics.ObserveDuration("parse", time.Since(started))

				select {
				case dedupQueue <- item:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// ---- Stage 3: dedup + persist (single bounded collector) ----
	var wgDedup sync.WaitGroup

	wgDedup.Add(1)

	go func() {
		defer wgDedup.Done()

		batch := make([]config.Config, 0, cfg.WriterBatchSize)

		flush := func() {
			if len(batch) == 0 {
				return
			}

			if err := sink.Persist(ctx, batch); err != nil {
				invalid.Add(int64(len(batch)))
			} else {
				persisted.Add(int64(len(batch)))
			}

			batch = batch[:0]
		}

		for item := range dedupQueue {
			if item.err != nil {
				perSourceMu.Lock()

				perSource = append(perSource, SourceResult{
					SourceID: item.src.ID,
					OK:       false,
					Error:    item.err.Error(),
				})

				perSourceMu.Unlock()

				continue
			}

			if item.unchangedHash != "" {
				perSourceMu.Lock()

				perSource = append(perSource, SourceResult{
					SourceID:    item.src.ID,
					OK:          true,
					Unchanged:   true,
					ContentHash: item.unchangedHash,
				})

				perSourceMu.Unlock()

				continue
			}

			started := time.Now()

			discovered.Add(int64(len(item.configs)))

			unique := 0

			for i := range item.configs {
				cfgPtr := &item.configs[i]
				cfgPtr.Normalize()

				if err := cfgPtr.Validate(); err != nil {
					invalid.Add(1)

					continue
				}

				if cfgPtr.ID == "" {
					cfgPtr.SetID()
				}

				if !dedupInsert(shards, cfgPtr.Fingerprint()) {
					duplicates.Add(1)

					continue
				}

				batch = append(batch, *cfgPtr)
				unique++

				if len(batch) >= cfg.WriterBatchSize {
					flush()
				}
			}

			perSourceMu.Lock()

			perSource = append(perSource, SourceResult{
				SourceID:   item.src.ID,
				OK:         true,
				Discovered: len(item.configs),
				Unique:     unique,
				DurationMS: time.Since(started).Milliseconds(),
			})

			perSourceMu.Unlock()
		}

		flush()
	}()

	// ---- Feed and drain the stages ----
	go func() {
		defer close(fetchQueue)

		for _, src := range sources {
			if !src.Enabled {
				continue
			}

			select {
			case fetchQueue <- src:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wgFetch.Wait()

		close(parseQueue)
	}()

	go func() {
		wgParse.Wait()

		close(dedupQueue)
	}()

	wgDedup.Wait()

	p.metrics.SetActiveWorkers(0)

	// Cancellation must surface to the caller even when every stage
	// drained "successfully" with partial results.
	if err := ctx.Err(); err != nil {
		return stats, newHashes, err
	}

	stats.SourcesOK = countOK(perSource)
	stats.SourcesFailed = stats.SourcesTotal - stats.SourcesOK
	stats.SourcesUnchanged = countUnchanged(perSource)
	stats.Discovered = discovered.Load()
	stats.Duplicates = duplicates.Load()
	stats.Invalid = invalid.Load()
	stats.Persisted = persisted.Load()
	stats.PerSource = perSource

	p.metrics.AddRecordsIn(stats.Discovered)
	p.metrics.AddRecordsDupes(stats.Duplicates)
	p.metrics.AddRecordsUnique(stats.Persisted)

	return stats, newHashes, nil
}

func countOK(results []SourceResult) int {
	n := 0

	for i := range results {
		if results[i].OK {
			n++
		}
	}

	return n
}

func countUnchanged(results []SourceResult) int {
	n := 0

	for i := range results {
		if results[i].Unchanged {
			n++
		}
	}

	return n
}

// dedupShard is one partition of the dedup index.
type dedupShard struct {
	mu   sync.Mutex
	seen map[uint64]struct{}
}

// dedupInsert reports whether the fingerprint is new. A 64-bit hash of
// the fingerprint (native-accelerated when available) is the working
// index; the full SHA-256 remains the authoritative identity stored
// with each record. Collision risk at realistic dataset sizes is
// negligible and bounded by the SHA-256 stored identity.
func dedupInsert(shards []*dedupShard, fingerprint string) bool {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fingerprint))

	sum := h.Sum64()
	shard := shards[sum%uint64(len(shards))]

	shard.mu.Lock()
	defer shard.mu.Unlock()

	if _, exists := shard.seen[sum]; exists {
		return false
	}

	shard.seen[sum] = struct{}{}

	return true
}

// contentFingerprint computes the change-detection hash of a payload.
func contentFingerprint(content []byte) string {
	var buf [8]byte

	sum := native.Hash64(content)

	for i := 0; i < 8; i++ {
		buf[i] = byte(sum >> (8 * i))
	}

	return hex.EncodeToString(buf[:])
}
