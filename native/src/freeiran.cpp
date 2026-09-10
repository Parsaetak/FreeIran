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
 */
#include "freeiran.h"

#include <cstring>

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

} /* namespace */

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

} /* extern "C" */
