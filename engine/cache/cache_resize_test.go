package cache

import "testing"

// TestSetMaxEntriesShrinksAndGrows verifies the runtime cache-target
// adjustment used by the memory booster: shrinking evicts the oldest
// entries immediately, growing restores capacity, and a zero target
// disables the entry bound.
func TestSetMaxEntriesShrinksAndGrows(t *testing.T) {
	l := New("test", Options{MaxEntries: 8})

	for i := 0; i < 8; i++ {
		l.Put(key(i), []byte{byte(i)}, 1)
	}

	if l.Len() != 8 {
		t.Fatalf("len = %d, want 8", l.Len())
	}

	// Shrink to 3: the oldest 5 entries are evicted immediately.
	l.SetMaxEntries(3)

	if l.Len() != 3 {
		t.Fatalf("len after shrink = %d, want 3", l.Len())
	}

	// The survivors are the newest entries.
	for i := 5; i < 8; i++ {
		if _, ok := l.Get(key(i), 1); !ok {
			t.Errorf("newest entry %d missing after shrink", i)
		}
	}

	for i := 0; i < 5; i++ {
		if _, ok := l.Get(key(i), 1); ok {
			t.Errorf("oldest entry %d survived shrink", i)
		}
	}

	// Growth restores capacity.
	l.SetMaxEntries(6)

	for i := 8; i < 11; i++ {
		l.Put(key(i), []byte{byte(i)}, 1)
	}

	if l.Len() != 6 {
		t.Fatalf("len after growth = %d, want 6", l.Len())
	}

	// Zero hard-disables the layer (the pre-existing Put contract:
	// MaxEntries == 0 drops new puts).
	l.SetMaxEntries(0)

	for i := 100; i < 110; i++ {
		l.Put(key(i), []byte{byte(i)}, 1)
	}

	if l.Len() != 6 {
		t.Fatalf("len with disabled layer = %d, want 6 (new puts must be dropped)", l.Len())
	}
}

func key(i int) string {
	return "k" + string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('0'+i/10%10))
}
