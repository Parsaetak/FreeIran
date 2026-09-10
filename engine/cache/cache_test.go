package cache

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestBasicPutGet(t *testing.T) {
	l := New("test", Options{MaxEntries: 4})

	l.Put("a", []byte("1"), 1)

	v, ok := l.Get("a", 1)
	if !ok || string(v.([]byte)) != "1" {
		t.Fatalf("get a = %v, %v", v, ok)
	}
}

func TestGenerationInvalidation(t *testing.T) {
	l := New("test", Options{MaxEntries: 4})

	l.Put("a", "v1", 1)

	if _, ok := l.Get("a", 2); ok {
		t.Fatal("stale generation must not be served")
	}

	if l.Len() != 0 {
		t.Fatal("stale entry must be invalidated on access")
	}

	// Re-put under the new generation works.
	l.Put("a", "v2", 2)

	v, _ := l.Get("a", 2)
	if v != "v2" {
		t.Fatalf("v2 = %v", v)
	}
}

func TestLRUEviction(t *testing.T) {
	l := New("lru", Options{MaxEntries: 2})

	l.Put("a", "1", 0)
	l.Put("b", "2", 0)

	// Touch "a" so "b" becomes the least recently used.
	_, _ = l.Get("a", 0)

	l.Put("c", "3", 0)

	if _, ok := l.Get("b", 0); ok {
		t.Fatal("b should have been evicted")
	}

	if _, ok := l.Get("a", 0); !ok {
		t.Fatal("a should still be present")
	}

	stats := l.Snapshot()

	if stats.Evictions != 1 {
		t.Fatalf("evictions = %d, want 1", stats.Evictions)
	}
}

func TestByteBudgetEviction(t *testing.T) {
	l := New("bytes", Options{
		MaxEntries: 100,
		MaxBytes:   200,
		Weigh:      ByteWeight,
	})

	// Each entry is 2 bytes payload + 32 overhead = 34.
	for i := 0; i < 20; i++ {
		l.Put(string(rune('a'+i)), make([]byte, 2), 0)
	}

	stats := l.Snapshot()

	if stats.Bytes > 200+40 { // budget + single-entry grace
		t.Fatalf("bytes = %d exceeds budget", stats.Bytes)
	}

	if stats.Entries >= 20 {
		t.Fatal("entries should have been evicted")
	}
}

func TestTTLExpiry(t *testing.T) {
	l := New("ttl", Options{MaxEntries: 4, TTL: 5 * time.Millisecond})

	l.Put("a", "1", 0)

	if _, ok := l.Get("a", 0); !ok {
		t.Fatal("entry should be alive")
	}

	time.Sleep(10 * time.Millisecond)

	if _, ok := l.Get("a", 0); ok {
		t.Fatal("expired entry must not be served")
	}
}

func TestHitMissStats(t *testing.T) {
	l := New("stats", Options{MaxEntries: 4})

	l.Put("a", "1", 0)

	_, _ = l.Get("a", 0)
	_, _ = l.Get("missing", 0)

	s := l.Snapshot()

	if s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("hits=%d misses=%d", s.Hits, s.Misses)
	}

	if s.HitRate < 0.499 || s.HitRate > 0.501 {
		t.Fatalf("hit rate = %v", s.HitRate)
	}
}

func TestInvalidateAndClear(t *testing.T) {
	l := New("clear", Options{MaxEntries: 4})

	l.Put("a", "1", 0)
	l.Put("b", "2", 0)

	l.Invalidate("a")

	if _, ok := l.Get("a", 0); ok {
		t.Fatal("a should be invalidated")
	}

	l.Clear()

	if l.Len() != 0 {
		t.Fatalf("len = %d after clear", l.Len())
	}
}

func TestDisabledLayer(t *testing.T) {
	l := New("off", Options{MaxEntries: 0})

	l.Put("a", "1", 0)

	if _, ok := l.Get("a", 0); ok {
		t.Fatal("disabled layer must not store anything")
	}

	if l.Len() != 0 {
		t.Fatal("disabled layer must be empty")
	}
}

func TestConcurrentAccess(t *testing.T) {
	l := New("concurrent", Options{MaxEntries: 64})

	var wg sync.WaitGroup

	for g := 0; g < 8; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			for i := 0; i < 500; i++ {
				key := string(rune('a' + i%26))

				l.Put(key, i, uint64(i))
				_, _ = l.Get(key, uint64(i))
			}
		}(g)
	}

	wg.Wait()
}

func TestStatsSerializable(t *testing.T) {
	l := New("json", Options{MaxEntries: 2})
	l.Put("a", "1", 0)

	data, err := json.Marshal(l.Snapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("empty stats json")
	}
}
