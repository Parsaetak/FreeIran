package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// serveBlob serves an in-memory blob with byte-range support.
func serveBlob(blob []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" {
			var start, end int64

			if n, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err == nil && n == 2 {
				w.Header().Set("Content-Range",
					fmt.Sprintf("bytes %d-%d/%d", start, end, len(blob)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(blob[start : end+1])

				return
			}

			if n, err := fmt.Sscanf(rng, "bytes=%d-", &start); err == nil && n == 1 {
				w.Header().Set("Content-Range",
					fmt.Sprintf("bytes %d-%d/%d", start, len(blob)-1, len(blob)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(blob[start:])

				return
			}

			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)

			return
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
		_, _ = w.Write(blob)
	})
}

// BenchmarkDownloadSingleStream measures the default data-plane path.
func BenchmarkDownloadSingleStream(b *testing.B) {
	blob := make([]byte, 64<<20) // 64 MiB
	for i := range blob {
		blob[i] = byte(i % 251)
	}

	srv := httptest.NewServer(serveBlob(blob))
	defer srv.Close()

	c := NewClient(Policy{MaxRetries: 1})
	defer c.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dest := b.TempDir() + "/bench.bin"

		if _, err := c.Download(context.Background(), srv.URL, DownloadOptions{
			DestPath:          dest,
			ExpectedSize:      int64(len(blob)),
			ParallelThreshold: 1 << 60, // parallel impossible: single stream
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDownloadParallelRanges measures the OPTIONAL ranged path
// (4 workers) on the same local payload. Localhost numbers measure
// the MECHANISM's overhead, not real-network speedups: they are
// recorded for honesty, not as a marketing claim.
func BenchmarkDownloadParallelRanges(b *testing.B) {
	blob := make([]byte, 64<<20)
	for i := range blob {
		blob[i] = byte(i % 251)
	}

	srv := httptest.NewServer(serveBlob(blob))
	defer srv.Close()

	c := NewClient(Policy{MaxRetries: 1})
	defer c.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dest := b.TempDir() + "/bench.bin"

		if _, err := c.Download(context.Background(), srv.URL, DownloadOptions{
			DestPath:          dest,
			ExpectedSize:      int64(len(blob)),
			ParallelThreshold: 8 << 20, // 64 MiB >= threshold: 4 workers
		}); err != nil {
			b.Fatal(err)
		}
	}
}
