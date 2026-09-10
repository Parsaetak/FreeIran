package chunks

import (
	"os"
	"path/filepath"
	"testing"
)

func benchRecords(n int) [][]byte {
	records := make([][]byte, n)

	for i := range records {
		record := make([]byte, 32+120)
		record[0] = byte(i)

		records[i] = record
	}

	return records
}

// BenchmarkChunkWrite measures writing one 4k-record chunk (temp +
// fsync + rename).
func BenchmarkChunkWrite(b *testing.B) {
	dir := b.TempDir()
	records := benchRecords(4096)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()

		path := filepath.Join(dir, "bench.firc")

		b.StartTimer()

		if _, err := WriteChunk(path, records, 0); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()

		if err := removeFile(path); err != nil {
			b.Fatal(err)
		}

		b.StartTimer()
	}
}

// BenchmarkChunkRead measures a full sequential scan of one chunk.
func BenchmarkChunkRead(b *testing.B) {
	dir := b.TempDir()
	records := benchRecords(4096)

	path := filepath.Join(dir, "bench.firc")

	if _, err := WriteChunk(path, records, 0); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader, err := OpenReader(path)
		if err != nil {
			b.Fatal(err)
		}

		for {
			_, err := reader.Next()
			if err != nil {
				break
			}
		}

		if err := reader.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkChunkVerify measures full-chunk CRC verification.
func BenchmarkChunkVerify(b *testing.B) {
	dir := b.TempDir()
	records := benchRecords(4096)

	path := filepath.Join(dir, "bench.firc")

	if _, err := WriteChunk(path, records, 0); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader, err := OpenReader(path)
		if err != nil {
			b.Fatal(err)
		}

		if err := reader.Verify(); err != nil {
			b.Fatal(err)
		}

		if err := reader.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func removeFile(path string) error {
	return os.Remove(path)
}
