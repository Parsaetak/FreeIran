// Package native is the bridge between the Go engine and the optional
// C++ acceleration layer in /native.
//
// Contract:
//
//   - Every operation has a pure-Go implementation producing results
//     bit-for-bit identical to the C++ implementation. Correctness never
//     depends on the native layer being present.
//   - When the binary is built with `-tags native_accel` and CGO is
//     enabled, the C++ entry points are used. Otherwise — or when the
//     caller forces a fallback via FREEIRAN_NATIVE=off — the Go path is
//     used and the fallback is recorded in metrics.
//   - Both languages implement FNV-1a 64 and CRC-32 (IEEE); the Go
//     standard library provides both, and the C++ implementations are
//     verified against the same reference vectors in native/tests.
//
// All exported functions are safe for concurrent use and accept empty
// input.
package native

import (
	"hash/crc32"
	"hash/fnv"
	"os"
	"strings"
	"sync/atomic"
)

// Mode reports which implementation the bridge currently dispatches to.
type Mode string

const (
	// ModeGo is the always-available pure-Go path.
	ModeGo Mode = "go"

	// ModeNative is the C++ accelerated path (build-tag enabled).
	ModeNative Mode = "native"
)

var (
	// forcedFallback is set when FREEIRAN_NATIVE=off so operators can
	// disable acceleration at runtime without a rebuild.
	forcedFallback atomic.Bool
)

func init() {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("FREEIRAN_NATIVE")))

	if value == "off" || value == "0" || value == "false" {
		forcedFallback.Store(true)
	}
}

// Available reports whether the native path would be used for new work.
func Available() bool {
	return !forcedFallback.Load()
}

// Hash64 computes the FNV-1a 64-bit hash of data. Empty and nil input
// return the FNV-1a offset basis (14695981039346656037).
func Hash64(data []byte) uint64 {
	return fnv1a64(data)
}

// fnv1a64 is the pure-Go FNV-1a 64 implementation used by the fallback
// path. It matches native/src/freeiran.cpp exactly.
func fnv1a64(data []byte) uint64 {
	const (
		offsetBasis = uint64(14695981039346656037)
		prime       = uint64(1099511628211)
	)

	h := offsetBasis

	for _, b := range data {
		h ^= uint64(b)
		h *= prime
	}

	return h
}

// Hash64Batch hashes count packed strings laid out contiguously in
// data. offsets holds count+1 entries delimiting each string. It
// returns the hashes in input order.
func Hash64Batch(data []byte, offsets []uint32) []uint64 {
	count := len(offsets) - 1

	if count <= 0 {
		return nil
	}

	hashes := make([]uint64, count)

	if !useNative() {
		for i := 0; i < count; i++ {
			start, end := int(offsets[i]), int(offsets[i+1])

			if end < start {
				end = start
			}

			hashes[i] = fnv1a64(data[start:end])
		}

		return hashes
	}

	// hashBatchNative is provided by bridge_cgo.go when built with
	// the native_accel tag.
	hashBatchNative(data, offsets, hashes)

	return hashes
}

// CRC32 computes the IEEE CRC-32 checksum, incrementally extendable
// through seed. It is identical to hash/crc32 IEEE and to the C++
// implementation.
func CRC32(seed uint32, data []byte) uint32 {
	// The stdlib table implementation is the canonical reference; the
	// C++ layer implements the same polynomial and is verified against
	// the 0xCBF43926 check value.
	if useNative() {
		return crc32Native(seed, data)
	}

	return crc32Update(seed, data)
}

// crc32Update is the pure-Go path.
func crc32Update(seed uint32, data []byte) uint32 {
	table := crc32.MakeTable(crc32.IEEE)

	return crc32.Update(seed, table, data)
}

// ScanURLs scans text for lines starting with a supported configuration
// scheme and returns the byte offset of the START of each matching line
// (including any leading whitespace). The returned count always covers
// the full buffer even when the caller-supplied output buffer is
// smaller; in that case the first len(out) offsets are filled.
func ScanURLs(text []byte, out []uint32) (found int, filled []uint32) {
	if len(text) == 0 {
		return 0, out[:0]
	}

	if !useNative() {
		return scanURLsGo(text, out)
	}

	return scanURLsNative(text, out)
}

// schemeList must stay in sync with native/src/freeiran.cpp.
var schemeList = []string{
	"vless://", "vmess://", "trojan://", "ss://", "hysteria://",
	"hysteria2://", "hy2://", "tuic://", "socks://", "socks5://", "wg://",
}

func isHorizontalSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\v' || c == '\f' || c == '\r'
}

// scanURLsGo mirrors native/src/freeiran.cpp::fir_scan_urls.
func scanURLsGo(text []byte, out []uint32) (int, []uint32) {
	found := 0
	written := 0

	i := 0
	n := len(text)

	for i < n {
		lineStart := i

		for i < n && text[i] != '\n' {
			i++
		}

		lineEnd := i
		i++

		cursor := lineStart

		for cursor < lineEnd && isHorizontalSpace(text[cursor]) {
			cursor++
		}

		if cursor == lineEnd {
			continue
		}

		matched := false

		for _, scheme := range schemeList {
			if lineEnd-cursor < len(scheme) {
				continue
			}

			if string(text[cursor:cursor+len(scheme)]) == scheme {
				matched = true

				break
			}
		}

		if !matched {
			continue
		}

		found++

		if written < len(out) {
			out[written] = uint32(lineStart)
			written++
		}
	}

	return found, out[:written]
}

// useNative reports whether the native path should be taken, and
// records a fallback decision for the metrics layer when it is not.
func useNative() bool {
	if !nativeCompiled || forcedFallback.Load() {
		fallbackHook()

		return false
	}

	return true
}

// fallbackHook is invoked whenever an operation takes the Go path in a
// binary that was requested to use native acceleration. It is a hook
// for the metrics registry; default is a no-op.
var fallbackHook = func() {}

// SetFallbackHook installs a callback invoked on each Go-path
// dispatch when acceleration was requested but unavailable.
func SetFallbackHook(fn func()) {
	if fn == nil {
		fallbackHook = func() {}
		return
	}

	fallbackHook = fn
}

// Ensure the standard library is referenced even when the native path
// is compiled in (keeps imports stable across build tags).
var _ = fnv.New64a
