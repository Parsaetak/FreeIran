//go:build !native_accel

package native

import "unsafe"

// nativeCompiled reports whether this binary was built with the C++
// acceleration layer linked in. Without the native_accel build tag the
// pure-Go implementations are always used.
const nativeCompiled = false

// hashBatchNative is only available in native_accel builds.
func hashBatchNative(data []byte, offsets []uint32, out []uint64) {
	panic("native: hashBatchNative called without native_accel build tag")
}

// crc32Native is only available in native_accel builds.
func crc32Native(seed uint32, data []byte) uint32 {
	panic("native: crc32Native called without native_accel build tag")
}

// crc32BatchNative is only available in native_accel builds.
func crc32BatchNative(data []byte, offsets []uint32, out []uint32) {
	panic("native: crc32BatchNative called without native_accel build tag")
}

// scanURLsNative is only available in native_accel builds.
func scanURLsNative(text []byte, out []uint32) (int, []uint32) {
	panic("native: scanURLsNative called without native_accel build tag")
}

// --- Native arena stubs (pure-Go fallback) ---
//
// When the native layer is not compiled in, the arena falls back to a
// Go implementation backed by make([]byte, size). This is not as fast
// as the native bump allocator (each alloc is a heap allocation) but
// preserves the API contract. The Go fallback arena tracks allocations
// so Stats() returns sensible values, but Reset is a no-op (the Go GC
// reclaims individual buffers).

// nativeArenaCreate returns 0 in the stub build (no native arena).
// Callers must check for 0 and use the Go fallback Arena instead.
func nativeArenaCreate(maxBlocks uint32) uintptr {
	return 0
}

func nativeArenaAlloc(handle uintptr, size uint32) unsafe.Pointer {
	return nil
}

func nativeArenaReset(handle uintptr) {}

func nativeArenaDestroy(handle uintptr) {}

// ArenaStats is shared between the cgo and stub builds.
type ArenaStats struct {
	TotalAllocs    uint64
	TotalBytes     uint64
	BlocksInUse    uint64
	BlocksCapacity uint64
	BytesInUse     uint64
	BytesCapacity  uint64
}

func nativeArenaStats(handle uintptr) (ArenaStats, bool) {
	return ArenaStats{}, false
}
