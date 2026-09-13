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
	"sync"
	"sync/atomic"
	"unsafe"
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

	// fallbackHookPtr holds the optional metrics callback invoked when
	// an operation takes the Go path in a binary that requested native
	// acceleration. Stored as an atomic.Pointer so SetFallbackHook can
	// be called concurrently with useNative() without a data race.
	// A nil pointer means "no hook" (the default).
	fallbackHookPtr atomic.Pointer[func()]
)

// noFallbackHook is the sentinel returned by fallbackHookPtr.Load when
// no hook has been set. We store this at init so Load never returns nil
// and callers can always safely dereference.
var noFallbackHook = func() {}

func init() {
	fallbackHookPtr.Store(&noFallbackHook)

	value := strings.ToLower(strings.TrimSpace(os.Getenv("FREEIRAN_NATIVE")))

	if value == "off" || value == "0" || value == "false" {
		forcedFallback.Store(true)
	}
}

// Available reports whether the native path would be used for new work.
func Available() bool {
	return !forcedFallback.Load()
}

// SetForcedFallback disables or re-enables the native acceleration
// path at runtime (v0.9.1 developer setting "force Go fallback").
// When force is true every bridge operation takes the pure-Go path
// regardless of build tags — the same state the FREEIRAN_NATIVE=off
// environment switch produces at init. Safe for concurrent use.
func SetForcedFallback(force bool) {
	forcedFallback.Store(force)
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

// CRC32Batch hashes count packed strings laid out contiguously in
// data. offsets holds count+1 entries delimiting each string. Each
// string is hashed independently from seed 0 (NOT incremental across
// strings). Returns the checksums in input order.
func CRC32Batch(data []byte, offsets []uint32) []uint32 {
	count := len(offsets) - 1
	if count <= 0 {
		return nil
	}
	out := make([]uint32, count)
	if useNative() {
		crc32BatchNative(data, offsets, out)
		return out
	}
	crc32BatchGo(data, offsets, out)
	return out
}

// crc32BatchGo is the pure-Go fallback for batch CRC-32.
func crc32BatchGo(data []byte, offsets []uint32, out []uint32) {
	table := crc32.MakeTable(crc32.IEEE)
	for i := 0; i < len(out) && i+1 < len(offsets); i++ {
		start, end := int(offsets[i]), int(offsets[i+1])
		if end < start || end > len(data) {
			end = start
		}
		out[i] = crc32.ChecksumIEEE(data[start:end])
		_ = table // keep the table reference for parity with crc32Update
	}
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
// Safe for concurrent use: fallbackHookPtr is an atomic.Pointer.
func useNative() bool {
	if !nativeCompiled || forcedFallback.Load() {
		hook := fallbackHookPtr.Load()
		if hook != nil {
			(*hook)()
		}
		return false
	}

	return true
}

// SetFallbackHook installs a callback invoked on each Go-path
// dispatch when acceleration was requested but unavailable. Pass nil
// to restore the no-op default. Safe to call concurrently with useNative.
func SetFallbackHook(fn func()) {
	if fn == nil {
		fallbackHookPtr.Store(&noFallbackHook)
		return
	}
	fallbackHookPtr.Store(&fn)
}

// Ensure the standard library is referenced even when the native path
// is compiled in (keeps imports stable across build tags).
var _ = fnv.New64a

// --- Arena: native-accelerated or Go-fallback bump allocator ---
//
// Arena is a bounded-growth bump allocator for high-frequency
// short-lived buffers. When the native layer is compiled in
// (-tags native_accel), allocations come from a pre-allocated 64 KiB
// block pool and are reclaimed via Reset (bulk) or Destroy (whole).
// When the native layer is NOT compiled in, the Arena falls back to
// individual make([]byte, size) allocations tracked in a Go slice;
// Reset is a no-op in this mode (the Go GC reclaims individual
// buffers) but the API contract is preserved.
//
// Ownership: the caller creates an Arena via NewArena and must call
// Destroy when done. Pointers returned by Alloc are valid until the
// next Reset or Destroy. The caller must NOT retain a pointer across
// Reset/Destroy.

// Arena is a bounded-growth bump allocator.
type Arena struct {
	handle uintptr // native arena handle (0 in Go-fallback mode)

	// Go-fallback state (used when handle == 0). Guarded by mu.
	mu     sync.Mutex
	goBufs [][]byte
	stats  ArenaStats
}

// NewArena creates an arena with the given block cap. maxBlocks=0 uses
// the native default (256 blocks = 16 MiB). The arena is safe for
// concurrent Alloc calls.
func NewArena(maxBlocks uint32) *Arena {
	a := &Arena{}
	if useNative() {
		a.handle = nativeArenaCreate(maxBlocks)
	}
	if a.handle == 0 {
		// Go fallback: no fixed block cap, but track a soft cap for
		// stats parity.
		if maxBlocks == 0 {
			maxBlocks = 256
		}
		a.stats.BlocksCapacity = uint64(maxBlocks)
	}
	return a
}

// Alloc returns a pointer to size bytes of arena memory. The pointer
// is 16-byte aligned (native) or naturally aligned (Go fallback). The
// pointer is valid until the next Reset or Destroy. Returns nil if the
// arena is at capacity (native mode only).
func (a *Arena) Alloc(size int) []byte {
	if size < 0 {
		return nil
	}
	if a.handle != 0 {
		ptr := nativeArenaAlloc(a.handle, uint32(size))
		if ptr == nil {
			return nil
		}
		// Convert the unsafe.Pointer to a []byte without copying.
		return unsafe.Slice((*byte)(ptr), size)
	}
	// Go fallback.
	buf := make([]byte, size)
	a.mu.Lock()
	a.goBufs = append(a.goBufs, buf)
	a.stats.TotalAllocs++
	a.stats.TotalBytes += uint64(size)
	a.stats.BytesInUse += uint64(size)
	a.stats.BlocksInUse = uint64(len(a.goBufs))
	a.mu.Unlock()
	return buf
}

// Reset marks all blocks reusable. In native mode this is a bulk
// reclaim (no deallocation). In Go-fallback mode this drops the
// reference to all allocated buffers (the GC reclaims them); subsequent
// Allocs start fresh.
func (a *Arena) Reset() {
	if a.handle != 0 {
		nativeArenaReset(a.handle)
		return
	}
	a.mu.Lock()
	a.goBufs = nil
	a.stats.BytesInUse = 0
	a.stats.BlocksInUse = 0
	a.mu.Unlock()
}

// Destroy releases all arena memory. The Arena must not be used after
// Destroy.
func (a *Arena) Destroy() {
	if a.handle != 0 {
		nativeArenaDestroy(a.handle)
		a.handle = 0
		return
	}
	a.mu.Lock()
	a.goBufs = nil
	a.stats = ArenaStats{}
	a.mu.Unlock()
}

// Stats returns the current allocation statistics.
func (a *Arena) Stats() ArenaStats {
	if a.handle != 0 {
		s, ok := nativeArenaStats(a.handle)
		if ok {
			return s
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}
