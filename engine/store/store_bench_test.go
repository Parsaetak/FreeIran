package store

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkStoreUpsertFlush(b *testing.B) {
	s := openBenchStore(b)
	defer s.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := s.Upsert(testKey(i%5000), testValue(i%5000)); err != nil {
			b.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkStoreGet(b *testing.B) {
	s := openBenchStore(b)
	defer s.Close()

	for i := 0; i < 5000; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			b.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := s.Get(testKey(i % 5000)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreReopen(b *testing.B) {
	path := b.TempDir()

	s, err := Open(Options{Path: path})
	if err != nil {
		b.Fatal(err)
	}

	for i := 0; i < 20000; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			b.Fatal(err)
		}
	}

	if err := s.Close(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reopened, err := Open(Options{Path: path})
		if err != nil {
			b.Fatal(err)
		}

		reopened.Close()
	}
}

func BenchmarkStoreIterate(b *testing.B) {
	s := openBenchStore(b)
	defer s.Close()

	for i := 0; i < 20000; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			b.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		seen := 0

		err := s.Iterate(context.Background(), func(key string, value []byte) error {
			seen++

			return nil
		})

		if err != nil {
			b.Fatal(err)
		}

		if seen != 20000 {
			b.Fatalf("iterated %d", seen)
		}
	}
}

func openBenchStore(b *testing.B) *Store {
	b.Helper()

	s, err := Open(Options{
		Path:            b.TempDir(),
		MemtableRecords: 4096,
	})
	if err != nil {
		b.Fatal(err)
	}

	return s
}

var _ = fmt.Sprint
