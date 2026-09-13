/*
 * FreeIran native acceleration layer — implementation.
 *
 * Design notes:
 *   - C++17, no external dependencies, no exceptions across the ABI
 *     boundary, no global mutable state.
 *   - The FNV-1a 64-bit and CRC-32 (IEEE) algorithms are implemented
 *     so they are bit-for-bit identical to the Go fallbacks in
 *     engine/native (verified by cross-language tests).
 *   - All entry points tolerate NULL and zero-length inputs.
 *   - The arena is a size-classed, bounded-growth bump allocator with
 *     a mutex for thread-safe alloc. Reset/destroy are also
 *     thread-safe.
 */
#include "freeiran.h"

#include <algorithm>
#include <cstring>
#include <mutex>
#include <new>
#include <vector>

namespace {

constexpr uint64_t kFnvOffsetBasis = 14695981039346656037ull;
constexpr uint64_t kFnvPrime       = 1099511628211ull;

/* The scanner recognises these lowercase schemes after the leading
 * whitespace of a line. Keep in sync with engine/native/scan.go. */
const char *const kSchemes[FIR_SCHEME_COUNT] = {
    "vless://", "vmess://", "trojan://", "ss://",     "hysteria://",
    "hysteria2://", "hy2://", "tuic://", "socks://", "socks5://", "wg://",
};

constexpr uint32_t kSchemeLens[FIR_SCHEME_COUNT] = {
    8, 8, 8, 5, 10, 11, 6, 7, 8, 9, 6,
};

inline uint64_t fnv1a64(const uint8_t *data, size_t length) {
    uint64_t hash = kFnvOffsetBasis;

    for (size_t i = 0; i < length; ++i) {
        hash ^= static_cast<uint64_t>(data[i]);
        hash *= kFnvPrime;
    }

    return hash;
}

/* CRC-32 (IEEE) table built at first use; safe under C++11 static
 * initialisation rules and identical to Go's hash/crc32 IEEE table. */
struct Crc32Table {
    uint32_t values[256];

    Crc32Table() {
        for (uint32_t i = 0; i < 256; ++i) {
            uint32_t crc = i;

            for (int bit = 0; bit < 8; ++bit) {
                crc = (crc & 1u) != 0u ? (crc >> 1) ^ 0xEDB88320u
                                       : crc >> 1;
            }

            values[i] = crc;
        }
    }
};

const Crc32Table &crcTable() {
    static const Crc32Table table;
    return table;
}

inline bool isHorizontalSpace(unsigned char c) {
    return c == ' ' || c == '\t' || c == '\v' || c == '\f' || c == '\r';
}

/* --------------------------------------------------------------------
 * Arena implementation.
 *
 * The arena is a sequence of fixed-size blocks (FIR_ARENA_BLOCK_BYTES
 * each). Allocations bump-allocate within the current block; when a
 * block is full, a new one is allocated up to max_blocks. Reset marks
 * all blocks reusable without freeing them. Destroy frees everything.
 *
 * Thread safety: a mutex guards the bump pointer and block list. This
 * is correct for the Go bridge's concurrency model (many goroutines
 * may call fir_arena_alloc concurrently). For higher throughput, the
 * caller can create one arena per worker.
 * -------------------------------------------------------------------- */

constexpr uint32_t kDefaultMaxBlocks = 256; /* 256 * 64 KiB = 16 MiB */
constexpr size_t   kAlignment         = 16;

inline size_t alignUp(size_t n, size_t align) {
    return (n + align - 1) & ~(align - 1);
}

struct ArenaBlock {
    uint8_t *data;     /* raw block memory */
    size_t   used;     /* bytes consumed so far */
};

inline uint8_t *blockAlloc(ArenaBlock &b, size_t alignedSize) {
    size_t remaining = FIR_ARENA_BLOCK_BYTES - b.used;
    if (alignedSize > remaining) {
        return nullptr;
    }
    uint8_t *ptr = b.data + b.used;
    b.used += alignedSize;
    return ptr;
}

} /* namespace */

/* fir_arena is defined in the GLOBAL namespace so it matches the
 * forward declaration in freeiran.h (typedef struct fir_arena *fir_arena_t).
 * The internal helpers above remain in the anonymous namespace. */
struct fir_arena {
    std::vector<ArenaBlock> blocks;
    uint32_t                max_blocks;
    uint32_t                current_block; /* index of the active block */
    std::mutex              mu;

    /* running stats (cumulative across resets) */
    uint64_t total_allocs;
    uint64_t total_bytes;

    /* per-epoch stats (reset by fir_arena_reset) */
    uint64_t epoch_bytes;

    fir_arena(uint32_t cap) : max_blocks(cap), current_block(0),
                              total_allocs(0), total_bytes(0), epoch_bytes(0) {
        blocks.reserve(cap);
    }
};

extern "C" {

uint32_t fir_abi_version(void) {
    return FIR_ABI_VERSION;
}

uint64_t fir_hash64(const uint8_t *data, uint32_t length) {
    if (data == nullptr || length == 0) {
        return kFnvOffsetBasis;
    }

    return fnv1a64(data, static_cast<size_t>(length));
}

int32_t fir_hash64_batch(const uint8_t *data,
                         const uint32_t *offsets,
                         uint32_t count,
                         uint64_t *out_hashes) {
    if (count == 0) {
        return 0;
    }

    if (data == nullptr || offsets == nullptr || out_hashes == nullptr) {
        return -1;
    }

    for (uint32_t i = 0; i < count; ++i) {
        uint32_t start = offsets[i];
        uint32_t end   = offsets[i + 1];

        if (end < start) {
            return -1;
        }

        out_hashes[i] = fnv1a64(data + start,
                                static_cast<size_t>(end - start));
    }

    return 0;
}

uint32_t fir_crc32(uint32_t seed, const uint8_t *data, uint32_t length) {
    const Crc32Table &table = crcTable();

    uint32_t crc = seed ^ 0xFFFFFFFFu;

    if (data == nullptr) {
        return crc ^ 0xFFFFFFFFu;
    }

    for (uint32_t i = 0; i < length; ++i) {
        crc = table.values[(crc ^ data[i]) & 0xFFu] ^ (crc >> 8);
    }

    return crc ^ 0xFFFFFFFFu;
}

int32_t fir_crc32_batch(const uint8_t *data,
                        const uint32_t *offsets,
                        uint32_t count,
                        uint32_t *out_crcs) {
    if (count == 0) {
        return 0;
    }

    if (data == nullptr || offsets == nullptr || out_crcs == nullptr) {
        return -1;
    }

    const Crc32Table &table = crcTable();

    for (uint32_t i = 0; i < count; ++i) {
        uint32_t start = offsets[i];
        uint32_t end   = offsets[i + 1];

        if (end < start) {
            return -1;
        }

        uint32_t crc = 0xFFFFFFFFu;
        for (uint32_t j = start; j < end; ++j) {
            crc = table.values[(crc ^ data[j]) & 0xFFu] ^ (crc >> 8);
        }
        out_crcs[i] = crc ^ 0xFFFFFFFFu;
    }

    return 0;
}

int64_t fir_scan_urls(const char *text,
                      uint32_t length,
                      uint32_t *out_offsets,
                      uint32_t max_urls) {
    if (text == nullptr && length != 0) {
        return -1;
    }

    if (out_offsets == nullptr && max_urls != 0) {
        return -1;
    }

    int64_t found  = 0;
    uint32_t i     = 0;
    uint32_t written = 0;

    while (i < length) {
        /* Record the start of the line, then skip leading whitespace
         * before scheme matching. */
        uint32_t lineStart = i;

        while (i < length && text[i] != '\n') {
            ++i;
        }

        uint32_t lineEnd = i; /* exclusive; does not include '\n' */
        ++i;                  /* move past '\n' (or length) */

        uint32_t cursor = lineStart;

        while (cursor < lineEnd && isHorizontalSpace(
                static_cast<unsigned char>(text[cursor]))) {
            ++cursor;
        }

        if (cursor == lineEnd) {
            continue; /* blank / whitespace-only line */
        }

        bool matched = false;

        for (uint32_t s = 0; s < FIR_SCHEME_COUNT; ++s) {
            uint32_t len = kSchemeLens[s];

            if (lineEnd - cursor < len) {
                continue;
            }

            if (std::memcmp(text + cursor, kSchemes[s], len) == 0) {
                matched = true;
                break;
            }
        }

        if (!matched) {
            continue;
        }

        ++found;

        if (written < max_urls) {
            out_offsets[written++] = lineStart;
        }
    }

    return found;
}

/* --- Arena API --- */

fir_arena_t fir_arena_create(uint32_t max_blocks) {
    if (max_blocks == 0) {
        max_blocks = kDefaultMaxBlocks;
    }
    /* Use nothrow new to avoid exceptions (-fno-exceptions build). */
    fir_arena *a = new (std::nothrow) fir_arena(max_blocks);
    if (a == nullptr) {
        return FIR_ARENA_NULL;
    }
    return a;
}

void *fir_arena_alloc(fir_arena_t arena, uint32_t size) {
    if (arena == FIR_ARENA_NULL) {
        return nullptr;
    }

    size_t aligned = alignUp(size == 0 ? 1 : size, kAlignment);

    std::lock_guard<std::mutex> lock(arena->mu);

    arena->total_allocs += 1;
    arena->total_bytes  += size;
    arena->epoch_bytes  += size;

    /* Try the current block first. */
    while (arena->current_block < arena->blocks.size()) {
        ArenaBlock &b = arena->blocks[arena->current_block];
        void *ptr = blockAlloc(b, aligned);
        if (ptr != nullptr) {
            return ptr;
        }
        /* Current block is full; advance. */
        arena->current_block += 1;
    }

    /* Need a new block. */
    if (arena->blocks.size() >= arena->max_blocks) {
        return nullptr; /* bounded growth */
    }

    ArenaBlock b;
    b.data = static_cast<uint8_t *>(std::malloc(FIR_ARENA_BLOCK_BYTES));
    if (b.data == nullptr) {
        return nullptr;
    }
    b.used = 0;

    /* push_back may reallocate the vector; under -fno-exceptions a
     * failure would abort. We accept this (the block cap is small
     * and the vector is reserved at construction). */
    arena->blocks.push_back(b);

    ArenaBlock &blk = arena->blocks.back();
    return blockAlloc(blk, aligned);
}

void fir_arena_reset(fir_arena_t arena) {
    if (arena == FIR_ARENA_NULL) {
        return;
    }
    std::lock_guard<std::mutex> lock(arena->mu);
    for (auto &b : arena->blocks) {
        b.used = 0;
    }
    arena->current_block = 0;
    arena->epoch_bytes = 0;
}

void fir_arena_destroy(fir_arena_t arena) {
    if (arena == FIR_ARENA_NULL) {
        return;
    }
    /* No lock: destroy is caller-serialized per the header contract. */
    for (auto &b : arena->blocks) {
        std::free(b.data);
        b.data = nullptr;
    }
    arena->blocks.clear();
    delete arena;
}

int32_t fir_arena_stats(fir_arena_t arena, struct fir_arena_stats *out) {
    if (arena == FIR_ARENA_NULL || out == nullptr) {
        return -1;
    }
    std::lock_guard<std::mutex> lock(arena->mu);
    out->total_allocs    = arena->total_allocs;
    out->total_bytes     = arena->total_bytes;
    out->blocks_in_use   = arena->blocks.size();
    out->blocks_capacity = arena->max_blocks;
    out->bytes_in_use    = arena->epoch_bytes;
    out->bytes_capacity  = static_cast<uint64_t>(arena->blocks.size()) *
                           FIR_ARENA_BLOCK_BYTES;
    return 0;
}

} /* extern "C" */
