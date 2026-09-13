/*
 * FreeIran native acceleration layer — stable C ABI.
 *
 * This header is the single contract between the Go engine and the C++
 * performance layer. It exposes only C-compatible entry points with
 * flat value semantics:
 *
 *   - The caller owns ALL buffers. No function here allocates memory
 *     that the caller must free, and no function retains any pointer
 *     after returning.
 *   - Every function validates its arguments and returns a negative
 *     value on error instead of throwing across the ABI boundary.
 *   - Empty input (length 0 or NULL) is valid and returns a
 *     well-defined result.
 *   - All algorithms are bit-for-bit reproducible across platforms.
 *     The Go fallback implementations must produce identical results,
 *     which is verified by cross-language tests in engine/native.
 *
 * ABI version: 2
 *
 * v2 additions (arena + batch CRC32):
 *   - fir_arena_create / fir_arena_alloc / fir_arena_reset /
 *     fir_arena_destroy / fir_arena_stats
 *   - fir_crc32_batch
 *
 * v1 functions (hash, crc32, scan_urls) remain unchanged.
 */
#ifndef FREEIRAN_H
#define FREEIRAN_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define FIR_ABI_VERSION 2u

/* Well-known configuration URL schemes recognised by the scanner. */
#define FIR_SCHEME_COUNT 11u

/*
 * Returns the ABI version of the loaded library. The Go bridge refuses
 * to use the library when the version does not match its expectation.
 */
uint32_t fir_abi_version(void);

/*
 * FNV-1a 64-bit batch hashing.
 *
 * Hashes `count` packed byte strings laid out contiguously in `data`.
 * `offsets` holds `count + 1` entries: string i occupies
 * data[offsets[i] .. offsets[i+1]-1]. This layout avoids per-string
 * pointer arrays and lets very large batches be hashed with a single
 * pass over memory.
 *
 * Results are written to `out_hashes` (caller allocated, count entries).
 *
 * Returns 0 on success, -1 on invalid arguments.
 * Empty strings (zero length) hash to the FNV-1a offset basis.
 */
int32_t fir_hash64_batch(const uint8_t *data,
                         const uint32_t *offsets,
                         uint32_t count,
                         uint64_t *out_hashes);

/*
 * Single-shot FNV-1a 64-bit hash of a buffer.
 * Returns the hash value; hashing NULL/empty yields the offset basis.
 */
uint64_t fir_hash64(const uint8_t *data, uint32_t length);

/*
 * Standard CRC-32 (IEEE 802.3, reflected polynomial 0xEDB88320),
 * identical to Go's hash/crc32 IEEE variant.
 *
 * Returns the checksum of data[0..length-1]. A NULL/empty buffer
 * yields 0. The `seed` parameter allows incremental computation;
 * pass 0 for a fresh checksum.
 */
uint32_t fir_crc32(uint32_t seed, const uint8_t *data, uint32_t length);

/*
 * Batch CRC-32. Hashes `count` packed byte strings laid out
 * contiguously in `data`, using `offsets` (count+1 entries) the same
 * way as fir_hash64_batch. Each string is hashed independently from
 * seed 0 (i.e. NOT incremental across strings).
 *
 * Results are written to `out_crcs` (caller allocated, count entries).
 *
 * Returns 0 on success, -1 on invalid arguments.
 */
int32_t fir_crc32_batch(const uint8_t *data,
                        const uint32_t *offsets,
                        uint32_t count,
                        uint32_t *out_crcs);

/*
 * High-throughput configuration URL scanner.
 *
 * Scans `text` (length bytes) and reports the byte offset of the start
 * of every line whose first token begins with a supported configuration
 * scheme ("vless://", "vmess://", "trojan://", "ss://", "hysteria://",
 * "hysteria2://", "hy2://", "tuic://", "socks://", "socks5://", "wg://").
 *
 * Leading horizontal whitespace on a line is skipped before scheme
 * matching. Lines longer than 64 KiB are still scanned from their
 * start; only line-start offsets are reported.
 *
 * `out_offsets` (caller allocated) receives up to `max_urls` offsets.
 *
 * Returns the total number of matching lines found in the buffer.
 * If the buffer contains more matches than `max_urls`, the first
 * `max_urls` offsets are written and the FULL count is returned so
 * the caller can grow its buffer and rescan.
 * Returns -1 on invalid arguments.
 */
int64_t fir_scan_urls(const char *text,
                      uint32_t length,
                      uint32_t *out_offsets,
                      uint32_t max_urls);

/* ====================================================================
 * Native memory arena.
 *
 * An opaque, size-classed, bounded-growth arena for high-frequency
 * short-lived allocations. The arena is NOT a general-purpose
 * allocator: it hands out raw pointers from pre-allocated blocks and
 * reclaims them only via fir_arena_reset (bulk) or fir_arena_destroy
 * (whole-arena). Individual free() is NOT supported.
 *
 * Ownership contract:
 *   - fir_arena_create returns an opaque handle (FIR_ARENA_NULL on
 *     failure). The caller owns the handle until fir_arena_destroy.
 *   - fir_arena_alloc returns a raw pointer into the arena's internal
 *     block. The pointer is valid until fir_arena_reset or
 *     fir_arena_destroy. The caller must NOT free() it.
 *   - fir_arena_reset marks all blocks as reusable but does NOT
 *     deallocate them. Subsequent allocs reuse the memory.
 *   - fir_arena_destroy releases all memory and invalidates the
 *     handle. Use-after-destroy is undefined.
 *
 * Thread safety:
 *   - fir_arena_create / fir_arena_destroy are NOT thread-safe
 *     (caller must serialize).
 *   - fir_arena_alloc IS thread-safe (uses a mutex internally).
 *     For higher throughput, the caller can create one arena per
 *     worker goroutine (thread-local cache).
 *   - fir_arena_reset / fir_arena_stats are thread-safe but must not
 *     race with fir_arena_destroy.
 *
 * Bounded growth:
 *   - max_blocks caps the total number of allocated blocks. When the
 *     cap is reached, fir_arena_alloc returns NULL instead of growing
 *     unboundedly.
 * ==================================================================== */

/* Opaque arena handle. FIR_ARENA_NULL (0) represents "no arena". */
typedef struct fir_arena *fir_arena_t;
#define FIR_ARENA_NULL ((fir_arena_t)0)

/* Size classes (power-of-two). Allocations are rounded up to the
 * nearest class. This bounds internal fragmentation. */
#define FIR_ARENA_CLASS_COUNT 6u
#define FIR_ARENA_BLOCK_BYTES (64 * 1024) /* 64 KiB per block */

/* fir_arena_stats: allocation statistics for diagnostics. */
struct fir_arena_stats {
    uint64_t total_allocs;    /* number of fir_arena_alloc calls */
    uint64_t total_bytes;     /* sum of requested sizes */
    uint64_t blocks_in_use;   /* current block count */
    uint64_t blocks_capacity; /* max_blocks cap */
    uint64_t bytes_in_use;    /* bytes handed out since last reset */
    uint64_t bytes_capacity;  /* blocks_in_use * FIR_ARENA_BLOCK_BYTES */
};

/*
 * fir_arena_create: create a new arena with the given block cap.
 *   max_blocks: maximum number of 64 KiB blocks the arena will
 *     allocate. 0 means use a default (256 blocks = 16 MiB).
 * Returns FIR_ARENA_NULL on failure (invalid arg or OOM).
 */
fir_arena_t fir_arena_create(uint32_t max_blocks);

/*
 * fir_arena_alloc: allocate `size` bytes from the arena.
 *   size: requested bytes. 0 is valid and returns a valid (but
 *     unusable) pointer.
 * Returns a raw pointer, or NULL if the arena is at capacity.
 * The pointer is 16-byte aligned.
 */
void *fir_arena_alloc(fir_arena_t arena, uint32_t size);

/*
 * fir_arena_reset: mark all blocks as reusable. Does NOT deallocate
 * memory; subsequent allocs reuse the existing blocks. Resets
 * bytes_in_use to 0 but preserves total_allocs/total_bytes counters.
 */
void fir_arena_reset(fir_arena_t arena);

/*
 * fir_arena_destroy: release all memory and invalidate the handle.
 * The handle must not be used after this call.
 */
void fir_arena_destroy(fir_arena_t arena);

/*
 * fir_arena_stats: fill in the stats struct. Returns 0 on success,
 * -1 if arena is FIR_ARENA_NULL.
 */
int32_t fir_arena_stats(fir_arena_t arena, struct fir_arena_stats *out);

#ifdef __cplusplus
}
#endif

#endif /* FREEIRAN_H */
