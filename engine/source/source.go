package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultTimeout    = 20 * time.Second
	DefaultMaxBodySize = 10 << 20 // 10 MiB
)

// Source describes a remote configuration source.
type Source struct {
	ID   string
	Name string
	URL  string

	// Enabled controls whether the source participates in collection.
	Enabled bool
}

// Result contains the downloaded source content and metadata.
type Result struct {
	Source      Source
	Content     []byte
	FetchedAt   time.Time
	StatusCode  int
	ContentType string
}

// Fetcher downloads configuration sources.
type Fetcher struct {
	Client     *http.Client
	MaxBodySize int64
}

// NewFetcher creates a Fetcher with safe defaults.
func NewFetcher() *Fetcher {
	return &Fetcher{
		Client: &http.Client{
			Timeout: DefaultTimeout,
		},
		MaxBodySize: DefaultMaxBodySize,
	}
}

// Fetch downloads one source.
func (f *Fetcher) Fetch(ctx context.Context, src Source) (Result, error) {
	if strings.TrimSpace(src.URL) == "" {
		return Result{}, fmt.Errorf("source URL is empty")
	}

	if !src.Enabled {
		return Result{}, fmt.Errorf("source %q is disabled", src.ID)
	}

	client := f.Client
	if client == nil {
		client = &http.Client{
			Timeout: DefaultTimeout,
		}
	}

	maxBodySize := f.MaxBodySize
	if maxBodySize <= 0 {
		maxBodySize = DefaultMaxBodySize
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		src.URL,
		nil,
	)
	if err != nil {
		return Result{}, fmt.Errorf("create source request: %w", err)
	}

	req.Header.Set("User-Agent", "FreeIran/0.1")
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("fetch source %q: %w", src.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf(
			"source %q returned HTTP %d",
			src.ID,
			resp.StatusCode,
		)
	}

	reader := io.LimitReader(resp.Body, maxBodySize+1)

	content, err := io.ReadAll(reader)
	if err != nil {
		return Result{}, fmt.Errorf(
			"read source %q: %w",
			src.ID,
			err,
		)
	}

	if int64(len(content)) > maxBodySize {
		return Result{}, fmt.Errorf(
			"source %q exceeds maximum size of %d bytes",
			src.ID,
			maxBodySize,
		)
	}

	return Result{
		Source:      src,
		Content:     content,
		FetchedAt:   time.Now().UTC(),
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}
