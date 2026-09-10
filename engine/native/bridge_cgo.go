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
