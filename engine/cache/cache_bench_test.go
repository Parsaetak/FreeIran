package cache

// Benchmarks for the cache layer (§51): hot-path reads, mixed
// read/write pressure and eviction under bounds.

import (
	"fmt"
	"testing"
)

func benchCache(b *testing.B, entries int) *Layer {
	b.Helper()

	layer := New("bench", Options{
		MaxEntries: entries,
		Weigh:      ByteWeight,
		MaxBytes:   1 << 26,
		TTL:        0,
	})

	for i := 0; i < entries; i++ {
		layer.Put(fmt.Sprintf("key-%06d", i), []byte("payload-0123456789"), 0)
	}

	return layer
}

// BenchmarkCacheGet measures hit-path reads.
func BenchmarkCacheGet(b *testing.B) {
	layer := benchCache(b, 4096)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = layer.Get(fmt.Sprintf("key-%06d", i%4096), 0)
	}
}

// BenchmarkCachePut measures insertions over existing bounds.
func BenchmarkCachePut(b *testing.B) {
	layer := benchCache(b, 4096)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		layer.Put(fmt.Sprintf("new-%06d", i), []byte("payload-0123456789"), 0)
	}
}

// BenchmarkCacheMixed measures a 75/25 read/write mix.
func BenchmarkCacheMixed(b *testing.B) {
	layer := benchCache(b, 4096)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if i%4 == 0 {
			layer.Put(fmt.Sprintf("mixed-%06d", i), []byte("payload-0123456789"), 0)

			continue
		}

		_, _ = layer.Get(fmt.Sprintf("key-%06d", i%4096), 0)
	}
}
