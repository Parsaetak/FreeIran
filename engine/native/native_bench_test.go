package native

import (
	"bytes"
	"testing"
)

// BenchmarkHashBatch measures the batch hashing hot path. Run with
// `-tags native_accel` (and CGO_ENABLED=1 plus a built native library)
// to measure the C++ implementation; the default build measures the
// pure-Go path. Both must produce identical results (verified by the
// TestHash64Batch equivalence tests).
func BenchmarkHashBatch(b *testing.B) {
	const count = 4096
	const recordLen = 96

	data := make([]byte, count*recordLen)

	// Deterministic payload.
	for i := range data {
		data[i] = byte(i % 251)
	}

	offsets := make([]uint32, count+1)

	for i := 0; i < count; i++ {
		offsets[i+1] = uint32((i + 1) * recordLen)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		hashes := Hash64Batch(data, offsets)
		if len(hashes) != count {
			b.Fatal("hash count mismatch")
		}
	}
}

// BenchmarkHashBatchNative vs Go comparison is only meaningful when
// the native layer is compiled in; this benchmark reports the
// effective mode in its name suffix.
func BenchmarkHashBatchMode(b *testing.B) {
	mode := "go"

	if Available() && nativeCompiled {
		mode = "native"
	}

	b.Run(mode, func(b *testing.B) {
		const count = 4096
		const recordLen = 96

		data := make([]byte, count*recordLen)

		for i := range data {
			data[i] = byte(i % 251)
		}

		offsets := make([]uint32, count+1)

		for i := 0; i < count; i++ {
			offsets[i+1] = uint32((i + 1) * recordLen)
		}

		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			hashes := Hash64Batch(data, offsets)
			if len(hashes) != count {
				b.Fatal("hash count mismatch")
			}
		}
	})
}

// BenchmarkScanURLs measures subscription scanning.
func BenchmarkScanURLs(b *testing.B) {
	var buf bytes.Buffer

	line := "vless://uuid@example.com:443?encryption=none#node\n"

	for i := 0; i < 2000; i++ {
		buf.WriteString("# comment line that is not a config\n")
		buf.WriteString(line)
	}

	text := buf.Bytes()
	out := make([]uint32, 4096)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		found, _ := ScanURLs(text, out)
		if found != 2000 {
			b.Fatalf("found %d, want 2000", found)
		}
	}
}
