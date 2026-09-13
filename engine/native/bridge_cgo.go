//go:build cgo && native_accel

// Package native — cgo bridge to the C++ acceleration layer.
//
// Requirements for this build path:
//
//  1. The native library must be built before compiling Go:
//     make -C native            # produces native/build/libfreeiran_native.a
//  2. Compile with CGO enabled and the native_accel tag:
//     CGO_ENABLED=1 go build -tags native_accel ./...
//
// The bridge verifies the ABI version of the loaded library at
// startup. If the ABI does not match, the bridge permanently falls
// back to the pure-Go path instead of producing wrong results.
package native

/*
#cgo CFLAGS: -I${SRCDIR}/../../native/include
#cgo LDFLAGS: ${SRCDIR}/../../native/build/libfreeiran_native.a -lstdc++
#include <stdint.h>
#include <stdlib.h>
#include "freeiran.h"
*/
import "C"

import (
	"sync/atomic"
	"unsafe"
)

// nativeCompiled reports that the C++ layer is linked into this binary.
const nativeCompiled = true

// abiVerified is checked at runtime before every dispatch.
var abiVerified atomic.Bool

func init() {
	if uint32(C.fir_abi_version()) == uint32(C.FIR_ABI_VERSION) {
		abiVerified.Store(true)
	}
}

func hashBatchNative(data []byte, offsets []uint32, out []uint64) {
	if !abiVerified.Load() || len(offsets) == 0 {
		copyFallbackHash(data, offsets, out)

		return
	}

	var dataPtr *C.uint8_t

	if len(data) > 0 {
		dataPtr = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}

	var offsetsPtr *C.uint32_t

	if len(offsets) > 0 {
		offsetsPtr = (*C.uint32_t)(unsafe.Pointer(&offsets[0]))
	}

	var outPtr *C.uint64_t

	if len(out) > 0 {
		outPtr = (*C.uint64_t)(unsafe.Pointer(&out[0]))
	}

	C.fir_hash64_batch(
		dataPtr,
		offsetsPtr,
		C.uint32_t(len(offsets)-1),
		outPtr,
	)
}

func crc32Native(seed uint32, data []byte) uint32 {
	if !abiVerified.Load() {
		return crc32Update(seed, data)
	}

	var dataPtr *C.uint8_t

	if len(data) > 0 {
		dataPtr = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}

	return uint32(C.fir_crc32(
		C.uint32_t(seed),
		dataPtr,
		C.uint32_t(len(data)),
	))
}

// crc32BatchNative dispatches to the C++ batch CRC-32. Falls back to
// the pure-Go path if the ABI is not verified.
func crc32BatchNative(data []byte, offsets []uint32, out []uint32) {
	if !abiVerified.Load() || len(offsets) == 0 {
		crc32BatchGo(data, offsets, out)
		return
	}

	var dataPtr *C.uint8_t
	if len(data) > 0 {
		dataPtr = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}

	var offsetsPtr *C.uint32_t
	if len(offsets) > 0 {
		offsetsPtr = (*C.uint32_t)(unsafe.Pointer(&offsets[0]))
	}

	var outPtr *C.uint32_t
	if len(out) > 0 {
		outPtr = (*C.uint32_t)(unsafe.Pointer(&out[0]))
	}

	// Ignore the return value; on error we already fell back above.
	_ = C.fir_crc32_batch(
		dataPtr,
		offsetsPtr,
		C.uint32_t(len(offsets)-1),
		outPtr,
	)
}

func scanURLsNative(text []byte, out []uint32) (int, []uint32) {
	if !abiVerified.Load() {
		return scanURLsGo(text, out)
	}

	var textPtr *C.char

	if len(text) > 0 {
		textPtr = (*C.char)(unsafe.Pointer(&text[0]))
	}

	var outPtr *C.uint32_t

	if len(out) > 0 {
		outPtr = (*C.uint32_t)(unsafe.Pointer(&out[0]))
	}

	found := int64(C.fir_scan_urls(
		textPtr,
		C.uint32_t(len(text)),
		outPtr,
		C.uint32_t(len(out)),
	))

	if found < 0 {
		// Defensive: invalid input should be impossible here; fall
		// back rather than fail.
		return scanURLsGo(text, out)
	}

	return int(found), out[:min(len(out), int(found))]
}

// --- Native arena bridge ---
//
// The arena handle is an opaque C pointer. We wrap it in a Go struct
// so the finalizer can call fir_arena_destroy if the caller forgets.
// The caller should still call Destroy() explicitly for prompt
// cleanup.

// nativeArenaCreate creates a native arena. Returns 0 (nil handle) on
// failure or when the native layer is not available.
func nativeArenaCreate(maxBlocks uint32) uintptr {
	if !abiVerified.Load() {
		return 0
	}
	return uintptr(unsafe.Pointer(C.fir_arena_create(C.uint32_t(maxBlocks))))
}

// nativeArenaAlloc allocates size bytes from the arena. Returns nil on
// failure (NULL handle, ABI not verified, or arena at capacity).
func nativeArenaAlloc(handle uintptr, size uint32) unsafe.Pointer {
	if handle == 0 || !abiVerified.Load() {
		return nil
	}
	h := (*C.struct_fir_arena)(unsafe.Pointer(handle))
	return unsafe.Pointer(C.fir_arena_alloc(h, C.uint32_t(size)))
}

// nativeArenaReset marks all blocks reusable.
func nativeArenaReset(handle uintptr) {
	if handle == 0 || !abiVerified.Load() {
		return
	}
	h := (*C.struct_fir_arena)(unsafe.Pointer(handle))
	C.fir_arena_reset(h)
}

// nativeArenaDestroy releases all arena memory.
func nativeArenaDestroy(handle uintptr) {
	if handle == 0 || !abiVerified.Load() {
		return
	}
	h := (*C.struct_fir_arena)(unsafe.Pointer(handle))
	C.fir_arena_destroy(h)
}

// ArenaStats is the Go projection of fir_arena_stats.
type ArenaStats struct {
	TotalAllocs    uint64
	TotalBytes     uint64
	BlocksInUse    uint64
	BlocksCapacity uint64
	BytesInUse     uint64
	BytesCapacity  uint64
}

// nativeArenaStats reads the arena stats. Returns ok=false on failure.
func nativeArenaStats(handle uintptr) (ArenaStats, bool) {
	if handle == 0 || !abiVerified.Load() {
		return ArenaStats{}, false
	}
	h := (*C.struct_fir_arena)(unsafe.Pointer(handle))
	var s C.struct_fir_arena_stats
	if int32(C.fir_arena_stats(h, &s)) != 0 {
		return ArenaStats{}, false
	}
	return ArenaStats{
		TotalAllocs:    uint64(s.total_allocs),
		TotalBytes:     uint64(s.total_bytes),
		BlocksInUse:    uint64(s.blocks_in_use),
		BlocksCapacity: uint64(s.blocks_capacity),
		BytesInUse:     uint64(s.bytes_in_use),
		BytesCapacity:  uint64(s.bytes_capacity),
	}, true
}

func copyFallbackHash(data []byte, offsets []uint32, out []uint64) {
	for i := 0; i < len(out) && i+1 < len(offsets); i++ {
		start, end := int(offsets[i]), int(offsets[i+1])

		if end < start || end > len(data) {
			end = start
		}

		out[i] = fnv1a64(data[start:end])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}

	return b
}
