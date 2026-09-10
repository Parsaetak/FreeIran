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
 * ABI version: 1
 */
#ifndef FREEIRAN_H
#define FREEIRAN_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define FIR_ABI_VERSION 1u

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

#ifdef __cplusplus
}
#endif

#endif /* FREEIRAN_H */
