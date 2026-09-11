package logging

// Benchmarks for the runtime logging hot path (§51): full pipeline
// cost of a log entry (format + redact + ring + file write) and the
// redaction pattern cost in isolation.

import (
	"os"
	"testing"
)

// BenchmarkLogWriteFile measures a full Info() call writing through
// rotation, ring and file.
func BenchmarkLogWriteFile(b *testing.B) {
	dir, err := os.MkdirTemp("", "freeiran-logbench")
	if err != nil {
		b.Fatal(err)
	}

	defer os.RemoveAll(dir)

	logger, err := Open(Options{
		Dir:        dir,
		MaxBytes:   64 << 20,
		MaxBackups: 1,
	})
	if err != nil {
		b.Fatal(err)
	}

	defer logger.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		logger.Info("bench", "write", "storage flush completed in %d ms with %d records", 12, 4096)
	}
}

// BenchmarkLogWriteRingOnly measures in-memory entry cost (no file,
// as during log level filtering of the file path or post-Close).
func BenchmarkLogWriteRingOnly(b *testing.B) {
	dir, err := os.MkdirTemp("", "freeiran-logbench")
	if err != nil {
		b.Fatal(err)
	}

	defer os.RemoveAll(dir)

	logger, err := Open(Options{Dir: dir})
	if err != nil {
		b.Fatal(err)
	}

	_ = logger.Close() // file closed: writes hit the ring only

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		logger.Info("bench", "write", "core xray ready on 127.0.0.1:%d", 10808)
	}
}

// BenchmarkRedact measures the pattern-based redaction cost for
// typical diagnostic text.
func BenchmarkRedact(b *testing.B) {
	text := "connection via vless://11111111-1111-1111-1111-111111111111@srv.example.com:443?security=tls " +
		"established with latency 42 ms and password=secret123 in query state"

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = Redact(text)
	}
}
