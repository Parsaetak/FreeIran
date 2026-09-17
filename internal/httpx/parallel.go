package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
)

// downloadParallel implements the OPTIONAL adaptive multi-range
// optimization. It is only attempted when:
//
//   - the expected size is known and >= ParallelThreshold, AND
//   - the server demonstrably supports byte ranges (probed with a
//     zero-length Range request that must answer 206 + a valid
//     Content-Range).
//
// Design guarantees:
//
//   - deterministic offsets: the file is split into W contiguous,
//     evenly sized chunks computed from the size alone;
//   - per-range retry: every worker retries transient failures
//     independently, resuming inside its chunk from its slice file;
//   - automatic fallback: any unrecoverable worker failure falls back
//     to the single-stream engine, which resumes from the contiguous
//     prefix (slice 0) — the transfer never restarts more than needed;
//   - bounded memory: workers stream to per-chunk slice files that
//     are concatenated with a plain sequential copy.
//
// The return's "attempted" flag reports whether the parallel path ran
// at all (false = server does not support ranges; use single stream).
func (c *Client) downloadParallel(ctx context.Context, url string, o DownloadOptions, mon *progressMonitor) (*DownloadResult, bool, error) {
	// ---- Probe range support with a 1-byte request ----
	probeResp, err := c.openStream(ctx, url, o, http.Header{"Range": []string{"bytes=0-0"}})
	if err != nil {
		return nil, false, nil // transient probe failure: single stream retries properly
	}

	defer probeResp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(probeResp.Body, 1<<10))

	if probeResp.StatusCode != http.StatusPartialContent {
		return nil, false, nil // ranges unsupported: single stream
	}

	start, _, total, ok := parseContentRange(probeResp.Header.Get("Content-Range"))
	if !ok || start != 0 || total <= 0 {
		return nil, false, nil // malformed range support: single stream
	}

	if o.ExpectedSize > 0 && total != o.ExpectedSize {
		return nil, true, fmt.Errorf("%w (expected %d, server reports %d)",
			ErrSizeMismatch, o.ExpectedSize, total)
	}

	// ---- Deterministic chunking ----
	workers := adaptiveWorkers(total, o.ParallelThreshold, o.ParallelMaxWorkers)

	mon.setWorkers(workers)

	chunks := splitRange(total, workers)

	slices := make([]string, len(chunks))
	for i := range chunks {
		slices[i] = fmt.Sprintf("%s.part.%d", o.DestPath, i)
	}

	// ---- Per-range workers ----
	type workerResult struct {
		err error
	}

	results := make([]error, len(chunks))

	wg := sync.WaitGroup{}

	for i := range chunks {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			results[idx] = c.downloadRange(ctx, url, o, mon, slices[idx], chunks[idx])
		}(i)
	}

	wg.Wait()

	// ---- Failure handling: fall back to single-stream resume ----
	for i, rerr := range results {
		if rerr != nil {
			// Preserve the contiguous prefix (slice 0) as the .part
			// file so the fallback resumes instead of restarting.
			preserveContiguousPrefix(slices, o.DestPath)

			return nil, true, fmt.Errorf("httpx: range worker %d/%d failed: %w",
				i+1, len(chunks), rerr)
		}
	}

	// ---- Concatenate slices into the final file ----
	sha, bytes, cerr := concatenateSlices(slices, partPath(o.DestPath))
	if cerr != nil {
		return nil, true, cerr
	}

	if finErr := finalizeCompletedPart(partPath(o.DestPath), o.DestPath); finErr != nil {
		return nil, true, finErr
	}

	return &DownloadResult{
		Bytes:    bytes,
		SHA256:   sha,
		Parallel: true,
		Workers:  workers,
	}, true, nil
}

// downloadRange streams one [start,end] chunk into slicePath with
// per-range resume and transient-failure retries.
func (c *Client) downloadRange(ctx context.Context, url string, o DownloadOptions, mon *progressMonitor, slicePath string, chunk chunkRange) error {
	var (
		retries  int
		restarts int
	)

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("httpx: range worker cancelled: %w", err)
		}

		offset := partSize(slicePath)
		if offset > chunk.length() {
			// Stale slice from an aborted run: restart the chunk.
			if truncErr := truncatePart(slicePath); truncErr != nil {
				return truncErr
			}

			offset = 0
		}

		remaining := chunk.length() - offset
		if remaining == 0 {
			return nil // chunk already complete on disk
		}

		from := chunk.start + offset

		resp, err := c.openStream(ctx, url, o, http.Header{
			"Range": []string{fmt.Sprintf("bytes=%d-%d", from, chunk.end)},
		})
		if err != nil {
			if isRetryableStreamErr(err) && retries < o.MaxRetries {
				retries++
				mon.setRetries(retries)

				if werr := c.sleepBackoff(ctx, retries, extractRetryAfter(err)); werr != nil {
					return werr
				}

				continue
			}

			return fmt.Errorf("open range %d-%d: %w", from, chunk.end, err)
		}

		switch {
		case resp.StatusCode == http.StatusPartialContent: // 206
			start, _, totalFromRange, ok := parseContentRange(resp.Header.Get("Content-Range"))
			if !ok || start != from {
				resp.Body.Close()

				if restarts++; restarts > 2 {
					return errors.New("inconsistent Content-Range for range worker")
				}

				if truncErr := truncatePart(slicePath); truncErr != nil {
					return truncErr
				}

				continue
			}

			_ = totalFromRange

		case resp.StatusCode == http.StatusOK: // 200
			// Range ignored mid-transfer: this worker cannot proceed
			// safely; the parallel attempt falls back to single-stream.
			resp.Body.Close()

			return errors.New("server ignored the Range request for a range worker")

		default:
			resp.Body.Close()

			if isRetryableStreamStatus(resp.StatusCode) && retries < o.MaxRetries {
				retries++
				mon.setRetries(retries)

				if werr := c.sleepBackoff(ctx, retries, parseRetryAfter(resp.Header.Get("Retry-After"))); werr != nil {
					return werr
				}

				continue
			}

			return fmt.Errorf("range %d-%d: HTTP %d", from, chunk.end, resp.StatusCode)
		}

		n, _, streamErr := c.streamToPart(ctx, resp.Body, slicePath, offset, remaining, false, o, mon)

		resp.Body.Close()

		if streamErr == nil {
			if offset+n == chunk.length() {
				return nil
			}

			// Short read inside the chunk: retry the remainder.
			if retries < o.MaxRetries {
				retries++
				mon.setRetries(retries)

				if werr := c.sleepBackoff(ctx, retries, 0); werr != nil {
					return werr
				}

				continue
			}

			return fmt.Errorf("%w (range %d-%d got %d of %d bytes)",
				ErrSizeMismatch, chunk.start, chunk.end, offset+n, chunk.length())
		}

		if errors.Is(streamErr, ErrDownloadStalled) || isRetryableStreamErr(streamErr) {
			if retries < o.MaxRetries {
				retries++
				mon.setRetries(retries)

				if werr := c.sleepBackoff(ctx, retries, 0); werr != nil {
					return werr
				}

				continue
			}

			return fmt.Errorf("range stream failed after %d retries: %w", retries, streamErr)
		}

		return fmt.Errorf("range stream failed: %w", streamErr)
	}
}

// chunkRange is one contiguous byte range [start, end] (inclusive).
type chunkRange struct {
	start int64
	end   int64
}

func (c chunkRange) length() int64 { return c.end - c.start + 1 }

// splitRange divides [0,total) into n contiguous, evenly sized
// chunks. The boundaries are a pure function of (total, n) — the
// "deterministic offsets" guarantee.
func splitRange(total int64, n int) []chunkRange {
	chunks := make([]chunkRange, 0, n)

	base := total / int64(n)
	rem := total % int64(n)

	var cursor int64

	for i := 0; i < n; i++ {
		size := base
		if int64(i) < rem {
			size++
		}

		if size <= 0 {
			continue
		}

		chunks = append(chunks, chunkRange{start: cursor, end: cursor + size - 1})

		cursor += size
	}

	return chunks
}

// adaptiveWorkers picks 2–4 workers for a file of the given size: the
// worker count is a pure function of size and threshold.
func adaptiveWorkers(total, threshold int64, maxWorkers int) int {
	switch {
	case total >= 4*threshold:
		return clampWorkers(4, maxWorkers)
	case total >= 2*threshold:
		return clampWorkers(3, maxWorkers)
	default:
		return clampWorkers(2, maxWorkers)
	}
}

func clampWorkers(want, max int) int {
	if want > max {
		want = max
	}

	if want < 2 {
		want = 2
	}

	return want
}

// preserveContiguousPrefix turns slice 0 (any prefix length) into the
// single-stream .part file and removes the other slices, so the
// fallback resumes from the contiguous prefix instead of restarting.
func preserveContiguousPrefix(slices []string, dest string) {
	part := partPath(dest)

	if len(slices) == 0 {
		return
	}

	if size := partSize(slices[0]); size > 0 {
		_ = os.Rename(slices[0], part)
	}

	for _, s := range slices[1:] {
		_ = os.Remove(s)
	}
}

// removeSlices deletes every slice file (success path).
func removeSlices(slices []string) {
	for _, s := range slices {
		_ = os.Remove(s)
	}
}

// concatenateSlices joins the slice files in chunk order into dest,
// computing the SHA-256 of the assembled content while copying.
// Returns the digest and the total byte count.
func concatenateSlices(slices []string, dest string) (string, int64, error) {
	// Defensive: slices must already be in order; sort is a no-op
	// safety net against accidental reordering.
	ordered := append([]string(nil), slices...)
	sort.Strings(ordered)

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("httpx: concat open: %w", err)
	}

	hasher := sha256.New()

	var total int64

	// v0.9.6: the output handle is closed explicitly (and its close
	// error checked) BEFORE the slice files are removed and before
	// the caller renames the assembled .part into place. No deferred
	// close may straddle the finalization boundary: the descriptor
	// must be gone when finalizeCompletedPart opens the .part, and
	// the slices must only be deleted once the assembled content is
	// safely on disk.
	succeeded := false

	defer func() {
		if !succeeded {
			_ = out.Close()
		}
	}()

	for _, s := range ordered {
		f, err := os.Open(s)
		if err != nil {
			return "", 0, fmt.Errorf("httpx: concat read %s: %w", s, err)
		}

		n, err := io.Copy(io.MultiWriter(out, hasher), f)
		_ = f.Close()

		if err != nil {
			return "", 0, fmt.Errorf("httpx: concat copy %s: %w", s, err)
		}

		total += n
	}

	if serr := out.Sync(); serr != nil {
		return "", 0, fmt.Errorf("httpx: concat sync: %w", serr)
	}

	if cerr := out.Close(); cerr != nil {
		return "", 0, fmt.Errorf("httpx: concat close: %w", cerr)
	}

	// Assembled and synced: the slices are redundant now.
	removeSlices(slices)

	succeeded = true

	return hex.EncodeToString(hasher.Sum(nil)), total, nil
}
