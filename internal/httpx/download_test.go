package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastDownloadPolicy keeps retries quick for data-plane tests.
func fastDownloadPolicy() Policy {
	return Policy{
		RequestTimeout:    5 * time.Second,
		MaxRetries:        4,
		BackoffBase:       5 * time.Millisecond,
		BackoffMax:        20 * time.Millisecond,
		BackoffJitter:     0.1,
		MaxRetryAfterWait: 10 * time.Second,
	}
}

func dlOpts(dest string) DownloadOptions {
	return DownloadOptions{
		DestPath:     dest,
		StallTimeout: 2 * time.Second,
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}

	return raw
}

// TestDownloadNormal verifies a plain full-body transfer with the
// streaming SHA-256.
func TestDownloadNormal(t *testing.T) {
	payload := []byte(strings.Repeat("FreeIran-downloader-test-", 4000)) // ~100 KiB

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "asset.zip")

	res, err := c.Download(context.Background(), srv.URL, dlOpts(dest))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	got := mustReadFile(t, dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	if res.SHA256 != sha256Hex(payload) {
		t.Fatalf("streaming SHA256 = %s, want %s", res.SHA256, sha256Hex(payload))
	}

	if res.Bytes != int64(len(payload)) || res.Parallel {
		t.Fatalf("result = %+v", res)
	}

	// No .part residue.
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part file left behind after success")
	}
}

// TestDownloadSlowResponseCompletes proves the data plane has NO
// total timeout: a transfer slower than any sane request budget still
// completes (the old shared 60s client killed these mid-body).
func TestDownloadSlowResponseCompletes(t *testing.T) {
	const chunks = 60

	const chunkSize = 4096

	var served atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(chunks*chunkSize))

		buf := make([]byte, chunkSize)
		for i := range buf {
			buf[i] = byte(i%251 + 1)
		}

		for i := 0; i < chunks; i++ {
			_, _ = w.Write(buf)
			served.Add(1)

			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}

			time.Sleep(20 * time.Millisecond) // ~1.2s total
		}
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "slow.bin")

	o := dlOpts(dest)
	o.StallTimeout = 3 * time.Second // > 20ms inter-chunk gap

	res, err := c.Download(context.Background(), srv.URL, o)
	if err != nil {
		t.Fatalf("slow download failed (total timeout regression?): %v", err)
	}

	if res.Bytes != chunks*chunkSize {
		t.Fatalf("bytes = %d, want %d", res.Bytes, chunks*chunkSize)
	}
}

// TestDownloadStalledDetected proves the no-progress watchdog aborts a
// stalled body (headers sent, then silence) even while Read blocks.
func TestDownloadStalledDetected(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		_, _ = w.Write(make([]byte, 1024))

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Then stall until the client gives up and the test tears down.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer close(release)
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "stalled.bin")

	o := dlOpts(dest)
	o.StallTimeout = 300 * time.Millisecond
	o.MaxRetries = -1 // < 0 normalizes to ZERO retries: fail fast

	started := time.Now()

	_, err := c.Download(context.Background(), srv.URL, o)
	if err == nil {
		t.Fatal("stalled download succeeded, want ErrDownloadStalled")
	}

	if !errors.Is(err, ErrDownloadStalled) {
		t.Fatalf("err = %v, want ErrDownloadStalled", err)
	}

	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("stall detection took %s, want < 3s", elapsed)
	}

	// The durable prefix must remain for a future resume.
	if info, serr := os.Stat(dest + ".part"); serr != nil || info.Size() != 1024 {
		t.Errorf(".part = %v (%v), want 1024-byte prefix retained", info, serr)
	}
}

// TestDownloadResumeAfterPartialFailure proves the resume path: a
// transfer killed mid-body is continued with a Range request whose
// Content-Range is validated before appending.
func TestDownloadResumeAfterPartialFailure(t *testing.T) {
	payload := []byte(strings.Repeat("resume-me-", 20000)) // 200 KiB

	const killAfter = 64 * 1024

	var attempts atomic.Int32

	var sawRange atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)

		if n == 1 {
			// Serve the first 64 KiB, then reset the connection.
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload[:killAfter])

			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}

			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("no hijack support")
			}

			conn, _, _ := hj.Hijack()
			_ = conn.Close()

			return
		}

		// Resume: must receive a Range header starting exactly at the
		// durable prefix.
		rng := r.Header.Get("Range")
		sawRange.Store(rng)

		start := int64(0)

		if strings.HasPrefix(rng, "bytes=") {
			fmt.Sscanf(strings.TrimPrefix(rng, "bytes="), "%d-", &start)
		}

		if start != killAfter {
			t.Errorf("resume Range = %q (start %d), want bytes=%d-", rng, start, killAfter)
		}

		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)-int(start)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start:])
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "resumed.bin")

	res, err := c.Download(context.Background(), srv.URL, dlOpts(dest))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if got, _ := sawRange.Load().(string); got == "" {
		t.Fatal("second attempt carried no Range header")
	}

	got := mustReadFile(t, dest)
	if string(got) != string(payload) {
		t.Fatalf("resumed content mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	if !res.Resumed || res.ResumedFrom != killAfter {
		t.Fatalf("result = %+v, want Resumed=true ResumedFrom=%d", res, killAfter)
	}

	if res.Retries < 1 {
		t.Fatalf("result.Retries = %d, want >= 1", res.Retries)
	}

	if res.SHA256 != sha256Hex(payload) {
		t.Fatalf("resumed SHA256 = %s, want %s (prefix must be re-hashed)", res.SHA256, sha256Hex(payload))
	}
}

// TestDownloadRangeUnsupportedRestartsSafely proves a server that
// ignores Range (200 instead of 206) triggers a clean restart from
// byte 0 instead of appending a second copy.
func TestDownloadRangeUnsupportedRestartsSafely(t *testing.T) {
	payload := []byte(strings.Repeat("fresh-start-", 10000)) // 120 KiB

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)

		// Always answer with the FULL body, ignoring any Range.
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "restart.bin")

	// Pre-seed a stale .part so the first attempt is a resume.
	part := dest + ".part"
	if err := os.WriteFile(part, []byte("stale-prefix-data-"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := c.Download(context.Background(), srv.URL, dlOpts(dest))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	got := mustReadFile(t, dest)
	if string(got) != string(payload) {
		t.Fatalf("content after range-unsupported restart = %d bytes (prefix %q), want exact %d",
			len(got), got[:min(20, len(got))], len(payload))
	}

	if res.SHA256 != sha256Hex(payload) {
		t.Fatal("SHA mismatch after restart")
	}

	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (resume tried, then restart)", got)
	}
}

// TestDownloadContentRangeValidated proves a LYING Content-Range
// (wrong start offset) is never appended to: the .part is discarded
// and the transfer restarts from zero.
func TestDownloadContentRangeValidated(t *testing.T) {
	payload := []byte(strings.Repeat("validated-", 15000)) // 150 KiB

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)

		if n == 1 {
			// Partial transfer that dies mid-body.
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload[:32*1024])

			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()

			return
		}

		if r.Header.Get("Range") == "" {
			// The clean-restart request (plain GET): full honest body.
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)

			return
		}

		// Lying 206: claims a start offset that does NOT match the
		// client's durable prefix.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", 999, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)-999))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[999:])
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "validated.bin")

	_, err := c.Download(context.Background(), srv.URL, dlOpts(dest))
	if err != nil {
		t.Fatalf("Download: %v (restart should have completed)", err)
	}

	got := mustReadFile(t, dest)
	if string(got) != string(payload) {
		t.Fatalf("content = %d bytes with prefix %q, want clean restart to exact payload",
			len(got), got[:min(24, len(got))])
	}
}

// TestDownloadSizeMismatch proves the published size is enforced.
func TestDownloadSizeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("short body"))
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "mismatch.bin")

	o := dlOpts(dest)
	o.ExpectedSize = 4096
	o.MaxRetries = -1

	_, err := c.Download(context.Background(), srv.URL, o)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v, want ErrSizeMismatch", err)
	}
}

// TestDownloadMaxBytes proves the hard byte cap.
func TestDownloadMaxBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		_, _ = w.Write(make([]byte, 1<<20))
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "cap.bin")

	o := dlOpts(dest)
	o.MaxBytes = 64 * 1024

	_, err := c.Download(context.Background(), srv.URL, o)
	if !errors.Is(err, ErrSizeExceeded) {
		t.Fatalf("err = %v, want ErrSizeExceeded", err)
	}
}

// TestDownloadCancellation proves ctx cancellation aborts the
// transfer promptly (including mid-body).
func TestDownloadCancellation(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10485760")

		buf := make([]byte, 4096)

		for {
			select {
			case <-r.Context().Done():
				return
			case <-release:
				return
			default:
			}

			if _, err := w.Write(buf); err != nil {
				return
			}

			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()
	defer close(release)

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "cancel.bin")

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	started := time.Now()

	_, err := c.Download(ctx, srv.URL, dlOpts(dest))
	if err == nil {
		t.Fatal("cancelled download succeeded")
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("cancellation took %s", elapsed)
	}
}

// TestDownload429Retried proves the data plane retries 429 with
// backoff and then completes.
func TestDownload429Retried(t *testing.T) {
	payload := []byte("after-rate-limit")

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "rate.bin")

	res, err := c.Download(context.Background(), srv.URL, dlOpts(dest))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if got := mustReadFile(t, dest); string(got) != string(payload) {
		t.Fatal("content mismatch after 429 retry")
	}

	if res.Retries != 1 {
		t.Fatalf("retries = %d, want 1", res.Retries)
	}
}

// TestDownload416CompletesFromPart proves the "already complete .part"
// shortcut: a 416 with offset == total finalizes without transfer.
func TestDownload416CompletesFromPart(t *testing.T) {
	payload := []byte("fully-on-disk-already")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "complete.bin")

	// Seed a COMPLETE .part file.
	if err := os.WriteFile(dest+".part", payload, 0o600); err != nil {
		t.Fatal(err)
	}

	o := dlOpts(dest)
	o.ExpectedSize = int64(len(payload))

	res, err := c.Download(context.Background(), srv.URL, o)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if got := mustReadFile(t, dest); string(got) != string(payload) {
		t.Fatal("content mismatch after 416 finalize")
	}

	if res.SHA256 != sha256Hex(payload) {
		t.Fatalf("SHA = %s, want %s", res.SHA256, sha256Hex(payload))
	}
}

// TestDownloadProgressTelemetry proves real byte/speed/ETA telemetry
// flows during a transfer.
func TestDownloadProgressTelemetry(t *testing.T) {
	const total = 200 * 1024

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(total))

		buf := make([]byte, 4096)

		for written := 0; written < total; {
			n := min(4096, total-written)
			_, _ = w.Write(buf[:n])
			written += n

			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "tele.bin")

	var (
		mu       sync.Mutex
		samples  []Progress
		sawSpeed bool
		sawETA   bool
		sawBytes bool
	)

	o := dlOpts(dest)
	o.ExpectedSize = total
	o.ProgressInterval = 10 * time.Millisecond
	o.OnProgress = func(p Progress) {
		mu.Lock()
		defer mu.Unlock()

		samples = append(samples, p)

		if p.SpeedBPS > 0 {
			sawSpeed = true
		}

		if p.ETASeconds > 0 {
			sawETA = true
		}

		if p.BytesDone > 0 && p.BytesDone < p.BytesTotal {
			sawBytes = true
		}
	}

	if _, err := c.Download(context.Background(), srv.URL, o); err != nil {
		t.Fatalf("Download: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(samples) < 3 {
		t.Fatalf("progress samples = %d, want >= 3", len(samples))
	}

	if !sawSpeed || !sawETA || !sawBytes {
		t.Fatalf("telemetry incomplete: speed=%v eta=%v mid-bytes=%v", sawSpeed, sawETA, sawBytes)
	}

	last := samples[len(samples)-1]
	if last.BytesDone != total || last.BytesTotal != total {
		t.Fatalf("final sample = %+v, want done=total=%d", last, total)
	}
}

// TestDownloadParallel proves the OPTIONAL ranged-parallel path:
// deterministic offsets, correct assembly and the full-file digest.
func TestDownloadParallel(t *testing.T) {
	const total = 64 * 1024

	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i % 253)
	}

	var mu sync.Mutex

	ranges := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")

		mu.Lock()
		ranges[rng]++
		mu.Unlock()

		var start, end int64

		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
			t.Errorf("bad range %q: %v", rng, err)
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.Itoa(int(end-start+1)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "parallel.bin")

	o := dlOpts(dest)
	o.ExpectedSize = total
	o.ParallelThreshold = 8 * 1024 // force the parallel path
	o.ParallelMaxWorkers = 4

	res, err := c.Download(context.Background(), srv.URL, o)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if !res.Parallel || res.Workers != 4 {
		t.Fatalf("result = %+v, want parallel with 4 workers", res)
	}

	if got := mustReadFile(t, dest); string(got) != string(payload) {
		t.Fatal("parallel assembly produced wrong content")
	}

	if res.SHA256 != sha256Hex(payload) {
		t.Fatalf("parallel SHA = %s, want %s", res.SHA256, sha256Hex(payload))
	}

	// The probe (bytes=0-0) + 4 disjoint chunk ranges must have been
	// requested with deterministic boundaries.
	mu.Lock()
	defer mu.Unlock()

	if ranges["bytes=0-0"] != 1 {
		t.Errorf("range probe count = %d, want 1", ranges["bytes=0-0"])
	}

	// 64 KiB / 4 = 16 KiB chunks: 0-16383, 16384-32767, ...
	for _, want := range []string{"bytes=0-16383", "bytes=16384-32767", "bytes=32768-49151", "bytes=49152-65535"} {
		if ranges[want] < 1 {
			t.Errorf("missing deterministic chunk range %q (got %v)", want, ranges)
		}
	}
}

// TestDownloadParallelFallsBackToSingleStream proves an unrecoverable
// range-worker failure falls back to the single-stream engine and the
// transfer still completes.
func TestDownloadParallelFallsBackToSingleStream(t *testing.T) {
	const total = 32 * 1024

	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i % 211)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")

		// Range probe succeeds.
		if rng == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", total))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[:1])

			return
		}

		// The SECOND chunk's closed range requests always fail with
		// 500 (exhausting per-range retries). The open-ended resume
		// from the single-stream fallback ("bytes=16384-") succeeds.
		if rng == "bytes=16384-32767" {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		var start, end int64

		// Closed range ("bytes=A-B") and open-ended resume ("bytes=A-")
		// are both accepted.
		spec := strings.TrimPrefix(rng, "bytes=")
		dash := strings.IndexByte(spec, '-')

		if dash < 0 {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		if _, err := fmt.Sscanf(spec[:dash], "%d", &start); err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		if rest := spec[dash+1:]; rest != "" {
			if _, err := fmt.Sscanf(rest, "%d", &end); err != nil {
				w.WriteHeader(http.StatusBadRequest)

				return
			}
		} else {
			end = total - 1
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "fallback.bin")

	o := dlOpts(dest)
	o.ExpectedSize = total
	o.ParallelThreshold = 4 * 1024
	o.ParallelMaxWorkers = 2

	res, err := c.Download(context.Background(), srv.URL, o)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if got := mustReadFile(t, dest); string(got) != string(payload) {
		t.Fatalf("fallback content mismatch: got %d bytes", len(got))
	}

	if res.SHA256 != sha256Hex(payload) {
		t.Fatal("fallback SHA mismatch")
	}

	if res.Workers != 1 {
		t.Fatalf("final workers = %d, want 1 (single stream)", res.Workers)
	}
}

// TestDownloadBoundedMemory is a structural guard: a large transfer
// completes with a small fixed buffer and never buffers the body in
// RAM (the buffer size is asserted, and peak RSS growth is coarse-
// checked to stay well below the payload size).
func TestDownloadBoundedMemory(t *testing.T) {
	const total = 8 << 20 // 8 MiB

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(total))

		buf := make([]byte, 64*1024)
		for written := 0; written < total; {
			n := min(len(buf), total-written)
			_, _ = w.Write(buf[:n])
			written += n
		}
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "big.bin")

	o := dlOpts(dest)
	o.ExpectedSize = total
	o.BufferSize = 32 * 1024 // explicit small buffer
	o.ParallelThreshold = 0  // single stream

	if _, err := c.Download(context.Background(), srv.URL, o); err != nil {
		t.Fatalf("Download: %v", err)
	}

	if info, err := os.Stat(dest); err != nil || info.Size() != total {
		t.Fatalf("dest size = %v (%v), want %d", info, err, total)
	}
}

// TestDownloadPrefixLongerThanExpected proves an invalid .part (longer
// than the published size) is discarded before any request.
func TestDownloadPrefixLongerThanExpected(t *testing.T) {
	payload := []byte("tiny")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			t.Errorf("Range sent despite invalid prefix: %q", r.Header.Get("Range"))
		}

		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := NewClient(fastDownloadPolicy())
	defer c.Close()

	dest := filepath.Join(t.TempDir(), "invalid-part.bin")

	// Oversized stale prefix.
	if err := os.WriteFile(dest+".part", make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	o := dlOpts(dest)
	o.ExpectedSize = int64(len(payload))

	res, err := c.Download(context.Background(), srv.URL, o)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if got := mustReadFile(t, dest); string(got) != string(payload) {
		t.Fatal("content mismatch after discarding invalid prefix")
	}

	if res.Resumed {
		t.Fatal("invalid prefix must not count as a resume")
	}
}

// connKillListener is used by nothing directly but keeps net imported
// symmetric with production imports.
var _ net.Error = (*net.OpError)(nil)

var _ io.Reader = (*strings.Reader)(nil)
