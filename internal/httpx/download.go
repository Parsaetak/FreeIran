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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Parsaetak/FreeIran/internal/version"
)

// ErrDownloadStalled reports a transfer that made no progress for
// longer than DownloadOptions.StallTimeout (after all retries).
var ErrDownloadStalled = errors.New("httpx: download stalled (no progress)")

// ErrSizeExceeded reports a transfer that passed the hard byte cap.
var ErrSizeExceeded = errors.New("httpx: download exceeded the byte limit")

// ErrSizeMismatch reports a completed transfer whose size differs
// from the expected (published) size.
var ErrSizeMismatch = errors.New("httpx: downloaded size does not match the expected size")

// DownloadOptions tune one data-plane transfer.
type DownloadOptions struct {
	// DestPath is the FINAL path. The transfer streams into
	// DestPath+".part" and is renamed on completion, so a partial
	// transfer never corrupts an existing destination file.
	DestPath string

	// ExpectedSize is the published total size when known (-1
	// otherwise). When known it is enforced: the final file must match
	// exactly, and resume decisions use it.
	ExpectedSize int64

	// MaxBytes is the hard byte cap (default 2 GiB).
	MaxBytes int64

	// StallTimeout aborts a stream that delivers no bytes for this
	// long (default 30s). Unlike a total timeout this never kills a
	// healthy slow transfer.
	StallTimeout time.Duration

	// BufferSize is the streaming copy buffer (default 256 KiB).
	BufferSize int

	// Header entries are added to the request (e.g. Accept).
	Header map[string]string

	// MaxRetries bounds resume attempts after transient failures
	// (default 4). Each attempt resumes from the .part offset.
	MaxRetries int

	// OnProgress, when set, receives telemetry updates. It is invoked
	// from a dedicated monitor goroutine — never from the I/O loop —
	// and must be cheap and safe for concurrent use.
	OnProgress func(Progress)

	// ParallelThreshold: transfers with a KNOWN expected size of at
	// least this many bytes MAY use deterministic ranged parallel
	// workers (2–4). Zero disables parallelism (default 64 MiB, which
	// keeps every current core archive on the single-stream path).
	ParallelThreshold int64

	// ParallelMaxWorkers caps ranged workers (default 4, clamped 2–4).
	ParallelMaxWorkers int

	// ProgressInterval sets the telemetry sampling cadence (default
	// 100ms; tests tighten it).
	ProgressInterval time.Duration
}

// Progress is one telemetry sample of a running download.
type Progress struct {
	// BytesDone counts bytes on disk for this transfer (including the
	// portion resumed from the .part file).
	BytesDone int64 `json:"bytes_done"`

	// BytesTotal is the expected total (-1 when unknown).
	BytesTotal int64 `json:"bytes_total"`

	// ResumedFrom is the byte offset the transfer started from (0 for
	// a fresh download; nonzero after a resume).
	ResumedFrom int64 `json:"resumed_from"`

	// SpeedBPS is the measured aggregate throughput in bytes/second.
	SpeedBPS float64 `json:"speed_bps"`

	// ETASeconds estimates the remaining time (-1 when unknown).
	ETASeconds float64 `json:"eta_seconds"`

	// Retries counts resume attempts after transient failures.
	Retries int `json:"retries"`

	// Workers is the number of active range workers (1 = single
	// stream).
	Workers int `json:"workers"`
}

// DownloadResult describes a completed transfer.
type DownloadResult struct {
	Bytes       int64
	Resumed     bool
	ResumedFrom int64
	Retries     int
	Attempts    int
	Duration    time.Duration

	// SHA256 is the hex digest of the final file content, computed
	// incrementally while streaming (the existing .part prefix is
	// re-hashed on resume), so checksum verification costs no extra
	// full-file read.
	SHA256 string

	// Parallel reports whether ranged workers were used.
	Parallel bool
	Workers  int
}

// partPath is the staging path for DestPath.
func partPath(dest string) string { return dest + ".part" }

// Download streams url to o.DestPath with resume, stall detection,
// progress telemetry and bounded memory. It never applies a total
// timeout: a healthy-but-slow transfer always completes.
func (c *Client) Download(ctx context.Context, url string, o DownloadOptions) (*DownloadResult, error) {
	if o.DestPath == "" {
		return nil, errors.New("httpx: DestPath is required")
	}

	o = o.normalize()

	started := time.Now()

	// Existing .part prefix = resumable offset.
	var resumedFrom int64

	if info, err := os.Stat(partPath(o.DestPath)); err == nil && info.Size() > 0 {
		resumedFrom = info.Size()
	}

	// Size sanity: a .part longer than the expected file is invalid
	// and is discarded before any request is made.
	if o.ExpectedSize > 0 && resumedFrom > o.ExpectedSize {
		if err := os.Remove(partPath(o.DestPath)); err != nil {
			return nil, fmt.Errorf("httpx: discard invalid .part: %w", err)
		}

		resumedFrom = 0
	}

	mon := newProgressMonitor(o, resumedFrom)
	mon.start(ctx)
	mon.setWorkers(1)

	res, err := c.download(ctx, url, o, mon, resumedFrom)

	mon.stop()

	if err != nil {
		return nil, err
	}

	res.Duration = time.Since(started)

	// Resumed reflects EITHER a pre-existing .part prefix OR a
	// mid-transfer resume after a retry (the engine fills
	// ResumedFrom when a retry continued from a durable offset).
	if resumedFrom > 0 && res.ResumedFrom == 0 {
		res.ResumedFrom = resumedFrom
	}

	res.Resumed = res.ResumedFrom > 0

	return res, nil
}

// download dispatches to the parallel or single-stream engine.
func (c *Client) download(ctx context.Context, url string, o DownloadOptions, mon *progressMonitor, resumedFrom int64) (*DownloadResult, error) {
	if o.ParallelThreshold > 0 && o.ExpectedSize >= o.ParallelThreshold && resumedFrom == 0 {
		res, attempted, err := c.downloadParallel(ctx, url, o, mon)
		if attempted && err == nil {
			return res, nil
		}

		if attempted && err != nil {
			// Parallel failed unrecoverably: fall back to the
			// single-stream engine resuming from whatever contiguous
			// prefix survived. v0.9.6: rebase the aggregate telemetry
			// onto the durable prefix - the failed workers' aborted
			// bytes no longer count as progress, so the fallback
			// reports honest numbers.
			offset := partSize(partPath(o.DestPath))

			mon.resetTo(offset)

			return c.downloadSingle(ctx, url, o, mon, offset)
		}
	}

	return c.downloadSingle(ctx, url, o, mon, -1)
}

// downloadSingle streams url into DestPath+".part", resuming from the
// existing prefix (or from forceOffset when >= 0), validating every
// Content-Range before appending and restarting safely when a resume
// is unsupported or invalid.
//
// forceOffset semantics: -1 = use/derive the .part offset; >= 0 =
// resume from exactly this offset (0 = fresh transfer).
func (c *Client) downloadSingle(ctx context.Context, url string, o DownloadOptions, mon *progressMonitor, forceOffset int64) (*DownloadResult, error) {
	part := partPath(o.DestPath)

	var (
		retries   int
		attempts  int
		restarts  int
		offset    = forceOffset
		total     = o.ExpectedSize
		resumedAt int64 // first mid-transfer resume offset (0 = none)
	)

	if forceOffset < 0 {
		offset = partSize(part)
	} else if forceOffset > 0 {
		// Inherited resume (the parallel-fallback entry point): the
		// transfer continues from an explicitly supplied durable
		// offset, which counts as a resume in the result telemetry.
		resumedAt = forceOffset
	}

	markResume := func(from int64) {
		if from > 0 && resumedAt == 0 {
			resumedAt = from
			mon.setResumed(from)
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("httpx: download cancelled: %w", err)
		}

		attempts++

		// Range request when resuming. A fresh transfer is a plain GET
		// so range-less servers work on the first attempt.
		var header http.Header

		if offset > 0 {
			header = http.Header{"Range": []string{fmt.Sprintf("bytes=%d-", offset)}}
		}

		resp, err := c.openStream(ctx, url, o, header)
		if err != nil {
			if isRetryableStreamErr(err) && retries < o.MaxRetries {
				retries++
				mon.setRetries(retries)

				if werr := c.sleepBackoff(ctx, retries, extractRetryAfter(err)); werr != nil {
					return nil, werr
				}

				offset = partSize(part)
				markResume(offset)

				continue
			}

			return nil, fmt.Errorf("httpx: open stream: %w", err)
		}

		// ---- Classify the response against the resume intent ----
		switch {
		case resp.StatusCode == http.StatusPartialContent: // 206
			start, _, totalFromRange, ok := parseContentRange(resp.Header.Get("Content-Range"))
			if !ok || start != offset {
				// Invalid/unexpected Content-Range: NEVER append.
				resp.Body.Close()

				if restarts++; restarts > 2 {
					return nil, errors.New("httpx: server returned inconsistent Content-Range after restart")
				}

				if truncErr := truncatePart(part); truncErr != nil {
					return nil, truncErr
				}

				offset = 0
				mon.resetFresh()

				continue
			}

			if totalFromRange > 0 {
				if total > 0 && totalFromRange != total {
					// The file changed under us (new asset at the same
					// URL): discard and restart.
					resp.Body.Close()

					if truncErr := truncatePart(part); truncErr != nil {
						return nil, truncErr
					}

					if restarts++; restarts > 2 {
						return nil, errors.New("httpx: asset size changed between attempts")
					}

					offset = 0
					total = totalFromRange
					mon.resetFresh()

					continue
				}

				total = totalFromRange
			}

		case resp.StatusCode == http.StatusOK: // 200
			if offset > 0 {
				// Server ignored the Range header: the only safe move
				// is a fresh transfer from byte 0.
				resp.Body.Close()

				if restarts++; restarts > 2 {
					return nil, errors.New("httpx: server ignored Range after restart")
				}

				if truncErr := truncatePart(part); truncErr != nil {
					return nil, truncErr
				}

				offset = 0
				mon.resetFresh()

				continue
			}

			if resp.ContentLength > 0 {
				if total > 0 && resp.ContentLength != total {
					resp.Body.Close()

					return nil, fmt.Errorf("%w (expected %d, server reports %d)",
						ErrSizeMismatch, total, resp.ContentLength)
				}

				total = resp.ContentLength
			}

		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable: // 416
			resp.Body.Close()

			if total > 0 && offset == total {
				// The .part file is already complete: finalize it.
				if finErr := finalizeCompletedPart(part, o.DestPath); finErr != nil {
					return nil, finErr
				}

				sha, err := fileSHA256(o.DestPath)
				if err != nil {
					return nil, err
				}

				return &DownloadResult{
					Bytes:    total,
					Retries:  retries,
					Attempts: attempts,
					SHA256:   sha,
					Workers:  1,
				}, nil
			}

			// Offset beyond the file: stale .part, restart clean.
			if truncErr := truncatePart(part); truncErr != nil {
				return nil, truncErr
			}

			if restarts++; restarts > 2 {
				return nil, errors.New("httpx: unsatisfiable range after restart")
			}

			offset = 0
			mon.resetFresh()

			continue

		default:
			resp.Body.Close()

			if isRetryableStreamStatus(resp.StatusCode) && retries < o.MaxRetries {
				retries++
				mon.setRetries(retries)

				if werr := c.sleepBackoff(ctx, retries, parseRetryAfter(resp.Header.Get("Retry-After"))); werr != nil {
					return nil, werr
				}

				offset = partSize(part)
				markResume(offset)

				continue
			}

			return nil, fmt.Errorf("httpx: download %s: HTTP %d", redactURL(url), resp.StatusCode)
		}

		// ---- Stream the body into the .part file ----
		n, hash, streamErr := c.streamToPart(ctx, resp.Body, part, offset, -1, true, o, mon)

		resp.Body.Close()

		offset += n

		if streamErr == nil {
			// EOF: validate the final size when known.
			if total > 0 && offset < total {
				// Server closed the body early (short read). The
				// durable prefix lets us resume.
				if retries < o.MaxRetries {
					retries++
					mon.setRetries(retries)

					if werr := c.sleepBackoff(ctx, retries, 0); werr != nil {
						return nil, werr
					}

					continue
				}

				return nil, fmt.Errorf("%w (got %d of %d bytes)", ErrSizeMismatch, offset, total)
			}

			if offset > o.MaxBytes {
				return nil, fmt.Errorf("%w (%d > %d)", ErrSizeExceeded, offset, o.MaxBytes)
			}

			if finErr := finalizeCompletedPart(part, o.DestPath); finErr != nil {
				return nil, finErr
			}

			return &DownloadResult{
				Bytes:       offset,
				ResumedFrom: resumedAt,
				Retries:     retries,
				Attempts:    attempts,
				SHA256:      hash,
				Workers:     1,
			}, nil
		}

		// ---- Stream failed ----
		if errors.Is(streamErr, ErrDownloadStalled) || isRetryableStreamErr(streamErr) {
			if retries < o.MaxRetries {
				retries++
				mon.setRetries(retries)

				if werr := c.sleepBackoff(ctx, retries, 0); werr != nil {
					return nil, werr
				}

				// Resume from the durable offset.
				markResume(offset)

				continue
			}

			return nil, fmt.Errorf("httpx: stream failed after %d retries: %w", retries, streamErr)
		}

		return nil, fmt.Errorf("httpx: stream failed: %w", streamErr)
	}
}

// stallChunk is one read result delivered by the watchdog reader.
type stallChunk struct {
	n   int
	err error
	// bufIndex selects which ping-pong buffer holds the bytes.
	bufIndex int
}

// hash256 is the incremental hasher interface used by streamToPart
// (nil disables hashing for range workers, whose digest is computed
// once during slice concatenation).
type hash256 interface {
	io.Writer
	Sum(b []byte) []byte
}

// streamToPart copies the response body into part at offset with a
// no-progress stall watchdog, the hard byte cap and live progress
// accounting. It returns the bytes written this call and the FULL-file
// SHA-256 when the stream reached EOF (empty string when doHash is
// false).
//
// limit bounds the bytes this call may read (-1 = until EOF); the
// range workers use it to stop exactly at their chunk end. The
// watchdog runs reads on a helper goroutine so a body that blocks
// without delivering bytes (the "stalled response" failure mode) is
// detected even while Read is blocked. Two ping-pong buffers keep the
// helper and the writer race-free.
func (c *Client) streamToPart(
	ctx context.Context,
	body io.ReadCloser,
	part string,
	offset, limit int64,
	doHash bool,
	o DownloadOptions,
	mon *progressMonitor,
) (int64, string, error) {
	bufs := [][]byte{make([]byte, o.BufferSize), make([]byte, o.BufferSize)}

	// O_RDWR (not O_WRONLY): the resumed prefix is re-read from the
	// same handle to hash it before appending.
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 0, "", fmt.Errorf("httpx: open .part: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, "", fmt.Errorf("httpx: seek .part to %d: %w", offset, err)
	}

	// Hash state: re-hash the durable prefix so the final digest
	// covers the whole file even across resumes.
	var hasher hash256

	if doHash {
		hasher = sha256.New()

		if offset > 0 {
			if err := hashPrefix(f, hasher, offset); err != nil {
				return 0, "", fmt.Errorf("httpx: hash resumed prefix: %w", err)
			}
		}
	}

	// Watchdog reader: alternates between the two buffers; the writer
	// only ever touches the buffer the reader is NOT filling. stop is
	// closed by the writer on return; the reader exits after its
	// blocked Read unblocks (the caller closes the body right after
	// this function returns).
	chunks := make(chan stallChunk)
	stop := make(chan struct{})

	go func() {
		idx := 0

		for {
			n, readErr := body.Read(bufs[idx])

			select {
			case chunks <- stallChunk{n: n, err: readErr, bufIndex: idx}:
			case <-stop:
				return
			}

			if readErr != nil {
				return
			}

			idx ^= 1
		}
	}()

	defer close(stop)

	stallTimer := time.NewTimer(o.StallTimeout)
	defer func() { stallTimer.Stop() }()

	var written int64

	for {
		select {
		case <-ctx.Done():
			return written, "", fmt.Errorf("httpx: download cancelled: %w", ctx.Err())

		case <-stallTimer.C:
			// No bytes for StallTimeout — including time spent blocked
			// inside Read. Abort; the retry loop resumes from disk.
			return written, "", fmt.Errorf("%w (no bytes for %s)", ErrDownloadStalled, o.StallTimeout)

		case chunk := <-chunks:
			if chunk.n > 0 {
				data := bufs[chunk.bufIndex][:chunk.n]

				if limit >= 0 && written+int64(chunk.n) > limit {
					// Server delivered more than the requested range:
					// refuse rather than write past the boundary. The
					// caller closes the body.
					return written, "", fmt.Errorf(
						"httpx: server delivered %d bytes past the range limit %d",
						written+int64(chunk.n)-limit, limit)
				}

				if _, werr := f.Write(data); werr != nil {
					return written, "", fmt.Errorf("httpx: write .part: %w", werr)
				}

				if hasher != nil {
					_, _ = hasher.Write(data)
				}

				written += int64(chunk.n)
				offset += int64(chunk.n)

				mon.advance(int64(chunk.n))

				if !stallTimer.Reset(o.StallTimeout) {
					stallTimer = time.NewTimer(o.StallTimeout)
				}

				if offset > o.MaxBytes {
					return written, "", fmt.Errorf("%w (%d > %d)", ErrSizeExceeded, offset, o.MaxBytes)
				}

				if limit >= 0 && written == limit {
					// Chunk complete: stop reading and flush.
					if serr := f.Sync(); serr != nil {
						return written, "", fmt.Errorf("httpx: sync .part: %w", serr)
					}

					return written, digestOf(hasher), nil
				}
			}

			if chunk.err == nil {
				continue
			}

			if errors.Is(chunk.err, io.EOF) {
				// Sync before declaring success so a crash between
				// write and rename can never lose a "completed" file.
				if serr := f.Sync(); serr != nil {
					return written, "", fmt.Errorf("httpx: sync .part: %w", serr)
				}

				return written, digestOf(hasher), nil
			}

			return written, "", fmt.Errorf("httpx: read body: %w", chunk.err)
		}
	}
}

// digestOf renders the hex digest of a hasher ("" when nil).
func digestOf(h hash256) string {
	if h == nil {
		return ""
	}

	return hex.EncodeToString(h.Sum(nil))
}

// hashPrefix feeds the first n bytes of an open file into h, then
// restores the file offset to n.
func hashPrefix(f *os.File, h io.Writer, n int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	if _, err := io.CopyN(h, f, n); err != nil {
		_, _ = f.Seek(n, io.SeekStart)

		return err
	}

	_, err := f.Seek(n, io.SeekStart)

	return err
}

// finalizeCompletedPart moved to finalize.go in v0.9.6, where the
// Windows root cause of the v0.9.5 CI regression was fixed (the
// read-only open + Sync that FlushFileBuffers rejects on Windows).

// fileSHA256 hashes a completed file (used for the 416-already-
// complete shortcut and by callers that skip the streaming hash).
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("httpx: hash %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()

	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("httpx: hash %s: %w", path, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// truncatePart removes a .part file so the next attempt starts fresh.
func truncatePart(part string) error {
	if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("httpx: discard .part: %w", err)
	}

	return nil
}

// partSize reports the durable .part size (0 when absent).
func partSize(part string) int64 {
	if info, err := os.Stat(part); err == nil {
		return info.Size()
	}

	return 0
}

// openStream performs one data-plane GET with the extra headers. The
// caller MUST close resp.Body. No total timeout is applied: stall
// detection and cancellation are the data-plane's guardrails.
func (c *Client) openStream(ctx context.Context, url string, o DownloadOptions, extra http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("httpx: build download request: %w", err)
	}

	req.Header.Set("User-Agent", version.UserAgent())

	for k, v := range o.Header {
		req.Header.Set(k, v)
	}

	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpx: request: %w", err)
	}

	return resp, nil
}

// sleepBackoff waits before retry attempt n (1-based), honouring a
// server Retry-After when one was provided.
func (c *Client) sleepBackoff(ctx context.Context, attempt int, retryAfter time.Duration) error {
	var wait time.Duration

	if retryAfter > 0 {
		wait = retryAfter
		if wait > c.policy.MaxRetryAfterWait {
			return fmt.Errorf("%w (server asked for %s)", ErrRateLimited, wait)
		}
	} else {
		wait, _, _ = c.retryDelay(attempt, nil)
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("httpx: download cancelled during backoff: %w", ctx.Err())
	case <-time.After(wait):
		return nil
	}
}

// isRetryableStreamStatus mirrors retryableStatus for the data plane.
func isRetryableStreamStatus(code int) bool { return retryableStatus(code) }

// isRetryableStreamErr classifies stream-open errors for retries.
func isRetryableStreamErr(err error) bool {
	var rse *retryableStatusError
	if errors.As(err, &rse) {
		return true
	}

	return retryableErr(err)
}

// extractRetryAfter pulls a Retry-After out of a wrapped status error.
func extractRetryAfter(err error) time.Duration {
	var rse *retryableStatusError
	if errors.As(err, &rse) {
		return rse.retryAfter
	}

	return 0
}

// parseContentRange parses "bytes start-end/total" (total may be "*").
// ok is false when the header is missing or malformed.
func parseContentRange(v string) (start, end, total int64, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, 0, 0, false
	}

	lowered := strings.ToLower(v)

	if !strings.HasPrefix(lowered, "bytes ") && !strings.HasPrefix(lowered, "bytes=") {
		return 0, 0, 0, false
	}

	v = v[len("bytes"):]
	v = strings.TrimPrefix(v, "=")
	v = strings.TrimSpace(v)

	slash := strings.IndexByte(v, '/')
	if slash < 0 {
		return 0, 0, 0, false
	}

	rng, totalStr := v[:slash], v[slash+1:]

	dash := strings.IndexByte(rng, '-')
	if dash < 0 {
		return 0, 0, 0, false
	}

	start, err := strconv.ParseInt(strings.TrimSpace(rng[:dash]), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, 0, false
	}

	end, err = strconv.ParseInt(strings.TrimSpace(rng[dash+1:]), 10, 64)
	if err != nil || end < start {
		return 0, 0, 0, false
	}

	totalStr = strings.TrimSpace(totalStr)

	if totalStr != "*" {
		total, err = strconv.ParseInt(totalStr, 10, 64)
		if err != nil || total < 0 {
			return 0, 0, 0, false
		}
	} else {
		total = -1
	}

	return start, end, total, true
}

// normalize fills DownloadOptions defaults.
func (o DownloadOptions) normalize() DownloadOptions {
	if o.MaxBytes <= 0 {
		o.MaxBytes = 2 << 30 // 2 GiB
	}
	if o.StallTimeout <= 0 {
		o.StallTimeout = 30 * time.Second
	}
	if o.BufferSize <= 0 {
		o.BufferSize = 256 << 10
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 4
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.ParallelThreshold == 0 {
		o.ParallelThreshold = 64 << 20 // 64 MiB
	}
	if o.ParallelMaxWorkers <= 1 {
		o.ParallelMaxWorkers = 2
	}
	if o.ParallelMaxWorkers > 4 {
		o.ParallelMaxWorkers = 4
	}
	if o.ExpectedSize < 0 {
		o.ExpectedSize = -1
	}
	if o.ProgressInterval <= 0 {
		o.ProgressInterval = 100 * time.Millisecond
	}
	return o
}

// ---------------------------------------------------------------------------
// Progress monitor
// ---------------------------------------------------------------------------

// progressMonitor aggregates byte counters and emits Progress samples
// from one goroutine at a fixed cadence.
type progressMonitor struct {
	opts      DownloadOptions
	resumed   atomic.Int64
	bytesDone atomic.Int64
	retries   atomic.Int32
	workers   atomic.Int32
	cancel    context.CancelFunc
	done      chan struct{}
	startedAt time.Time
}

func newProgressMonitor(o DownloadOptions, resumedFrom int64) *progressMonitor {
	m := &progressMonitor{
		opts: o,
		done: make(chan struct{}),
	}
	m.resumed.Store(resumedFrom)

	return m
}

// start launches the sampling loop.
func (m *progressMonitor) start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)

	m.startedAt = time.Now()

	go func() {
		defer close(m.done)

		ticker := time.NewTicker(m.opts.ProgressInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			m.emit(m.sample(time.Since(m.startedAt)))
		}
	}()
}

// stop terminates sampling and emits one FINAL terminal sample so
// consumers observe the completed byte count even when the transfer
// finishes between ticks.
func (m *progressMonitor) stop() {
	if m.cancel != nil {
		m.cancel()
	}

	<-m.done

	m.emit(m.sample(time.Since(m.startedAt)))
}

// sample builds one Progress snapshot.
func (m *progressMonitor) sample(elapsed time.Duration) Progress {
	done := m.bytesDone.Load()
	total := m.opts.ExpectedSize

	seconds := elapsed.Seconds()

	var speed float64

	if seconds > 0 {
		speed = float64(done) / seconds
	}

	var eta float64 = -1

	if total > 0 && done < total && speed > 0 {
		eta = float64(total-done) / speed
	}

	return Progress{
		BytesDone:   done,
		BytesTotal:  total,
		ResumedFrom: m.resumed.Load(),
		SpeedBPS:    speed,
		ETASeconds:  eta,
		Retries:     int(m.retries.Load()),
		Workers:     int(m.workers.Load()),
	}
}

// advance adds n bytes to the aggregate.
func (m *progressMonitor) advance(n int64) { m.bytesDone.Add(n) }

// setRetries records the current retry count.
func (m *progressMonitor) setRetries(n int) { m.retries.Store(int32(n)) }

// setResumed records a mid-transfer resume offset.
func (m *progressMonitor) setResumed(from int64) { m.resumed.Store(from) }

// setWorkers records the active worker count.
func (m *progressMonitor) setWorkers(n int) { m.workers.Store(int32(n)) }

// resetFresh marks a restart from byte 0 (invalid resume discarded).
func (m *progressMonitor) resetFresh() {
	m.resumed.Store(0)
	m.bytesDone.Store(0)
}

// resetTo rebases aggregate accounting onto a durable on-disk offset.
// Used when the parallel engine falls back to single-stream: only the
// preserved contiguous prefix counts as progress from that point on.
func (m *progressMonitor) resetTo(offset int64) {
	m.resumed.Store(offset)
	m.bytesDone.Store(offset)
}

// emit delivers one sample to the callback (if any).
func (m *progressMonitor) emit(p Progress) {
	if m.opts.OnProgress == nil {
		return
	}

	defer func() { _ = recover() }() // a panicking sink must not kill the transfer

	m.opts.OnProgress(p)
}
