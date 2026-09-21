// v099_gencache_test.go — v0.9.9 cache-contract regression coverage:
//
//   - GenerationFor is truthful about backend VERSION awareness (the
//     pre-0.9.9 doc claimed it; the hash lacked it);
//   - a per-launch coordinate change (ports) creates a new KEY without
//     invalidating other candidates' entries;
//   - a backend-version change invalidates the whole generation;
//   - ResolveInboundPort is the single allocation authority.
package core

import (
	"sync"
	"testing"
)

func TestGenerationForVersionAwareness(t *testing.T) {
	same := GenerationFor("xray", "/cores/xray/bin/xray", "26.3.27")
	sameAgain := GenerationFor("xray", "/cores/xray/bin/xray", "26.3.27")

	if same != sameAgain {
		t.Fatal("identical inputs must produce identical generations")
	}

	upgraded := GenerationFor("xray", "/cores/xray/bin/xray", "26.9.11")

	if same == upgraded {
		t.Fatal("a backend VERSION change must invalidate the generation " +
			"(the pre-0.9.9 hash omitted the version entirely)")
	}

	newPath := GenerationFor("xray", "/cores/xray-new/bin/xray", "26.3.27")

	if same == newPath {
		t.Fatal("a binary path change must invalidate the generation")
	}
}

func TestGenCacheVersionInvalidation(t *testing.T) {
	cache := NewGenCache()

	key := GenCacheKey("fp-1", "xray", "127.0.0.1", 10800, 0)
	gen := GenerationFor("xray", "/bin/xray", "v1")

	cache.Put(key, gen, RuntimeConfig{FileName: "a.json"})

	if _, ok := cache.Get(key, gen); !ok {
		t.Fatal("entry must be present for the matching generation")
	}

	// Backend update: version changes → wholesale invalidation.
	upgraded := GenerationFor("xray", "/bin/xray", "v2")

	if _, ok := cache.Get(key, upgraded); ok {
		t.Fatal("stale-generation entry must not be served after a backend version change")
	}

	// The invalidation must also have CLEARED the old entry.
	if _, ok := cache.Get(key, gen); ok {
		t.Fatal("old-generation entry must be dropped after invalidation")
	}
}

func TestGenCachePortChangeDoesNotInvalidateOtherEntries(t *testing.T) {
	cache := NewGenCache()

	gen := GenerationFor("xray", "/bin/xray", "v1")

	keyA := GenCacheKey("fp-A", "xray", "127.0.0.1", 10800, 0)
	keyB := GenCacheKey("fp-B", "xray", "127.0.0.1", 10801, 0)

	cache.Put(keyA, gen, RuntimeConfig{FileName: "a.json"})
	cache.Put(keyB, gen, RuntimeConfig{FileName: "b.json"})

	// Candidate A reconnects on a NEW port: a different KEY, same
	// generation. Candidate B's entry must survive (the pre-0.9.9
	// generation folded the ports in, so ANY port change reset the
	// entire cache).
	keyANew := GenCacheKey("fp-A", "xray", "127.0.0.1", 10805, 0)

	if _, ok := cache.Get(keyANew, gen); ok {
		t.Fatal("a new port must be a cache MISS for that candidate")
	}

	cache.Put(keyANew, gen, RuntimeConfig{FileName: "a-new.json"})

	if got, ok := cache.Get(keyB, gen); !ok || got.FileName != "b.json" {
		t.Fatalf("candidate B's entry must survive candidate A's port change (got %v ok=%v)", got, ok)
	}

	if got, ok := cache.Get(keyA, gen); !ok || got.FileName != "a.json" {
		t.Fatalf("candidate A's old-port entry must remain usable within TTL (got %v ok=%v)", got, ok)
	}
}

func TestGenCacheConcurrentAccess(t *testing.T) {
	cache := NewGenCache()

	gen := GenerationFor("xray", "/bin/xray", "v1")

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()

			key := GenCacheKey("fp", "xray", "127.0.0.1", 20000+n, 0)

			for j := 0; j < 200; j++ {
				cache.Put(key, gen, RuntimeConfig{FileName: "x.json"})
				_, _ = cache.Get(key, gen)
			}
		}(i)
	}

	wg.Wait()
}

func TestResolveInboundPortPassthrough(t *testing.T) {
	// An explicitly requested port passes through untouched — the ONE
	// authority for user-selected ports (no allocation happens).
	port, err := ResolveInboundPort("127.0.0.1", 45678)
	if err != nil {
		t.Fatalf("ResolveInboundPort(45678) = %v", err)
	}

	if port != 45678 {
		t.Fatalf("explicit port mutated: got %d, want 45678", port)
	}
}

func TestResolveInboundPortAllocates(t *testing.T) {
	seen := map[int]bool{}

	for i := 0; i < 4; i++ {
		port, err := ResolveInboundPort("127.0.0.1", 0)
		if err != nil {
			t.Fatalf("ResolveInboundPort(0) = %v", err)
		}

		if port <= 0 {
			t.Fatalf("allocated port = %d, want a positive ephemeral port", port)
		}

		seen[port] = true
	}
}

// BenchmarkGenCacheRoundTrip measures Get+Put on the generated-config
// cache (the per-attempt cost every connect pays through the adapter
// BuildConfig path).
func BenchmarkGenCacheRoundTrip(b *testing.B) {
	cache := NewGenCache()

	gen := GenerationFor("xray", "/bin/xray", "v1")
	key := GenCacheKey("fp", "xray", "127.0.0.1", 10808, 0)

	doc := RuntimeConfig{FileName: "xray-bench.json", Data: make([]byte, 4096)}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		cache.Put(key, gen, doc)

		if _, ok := cache.Get(key, gen); !ok {
			b.Fatal("cache miss")
		}
	}
}
