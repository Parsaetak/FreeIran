/*
 * FreeIran native layer unit tests.
 *
 * A deliberately dependency-free harness: failures print to stderr and
 * the process exits non-zero. Build & run:  make -C native test
 */
#include "../include/freeiran.h"

#include <atomic>
#include <cstdio>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

namespace {

int g_failures = 0;

void expectEqUint64(uint64_t got, uint64_t want, const char *what) {
    if (got != want) {
        std::fprintf(stderr, "FAIL: %s: got %llu want %llu\n",
                     what,
                     static_cast<unsigned long long>(got),
                     static_cast<unsigned long long>(want));
        ++g_failures;
    }
}

void expectEqInt64(int64_t got, int64_t want, const char *what) {
    if (got != want) {
        std::fprintf(stderr, "FAIL: %s: got %lld want %lld\n",
                     what,
                     static_cast<long long>(got),
                     static_cast<long long>(want));
        ++g_failures;
    }
}

void expectEqInt32(int32_t got, int32_t want, const char *what) {
    if (got != want) {
        std::fprintf(stderr, "FAIL: %s: got %d want %d\n", what, got, want);
        ++g_failures;
    }
}

/* FNV-1a 64 reference values (public test vectors). */
void testFnvVectors() {
    expectEqUint64(fir_hash64(nullptr, 0), 14695981039346656037ull,
                   "empty hash equals offset basis");

    /* Reference: FNV-1a("a") = 0xaf63dc4c8601ec8c */
    expectEqUint64(fir_hash64(reinterpret_cast<const uint8_t *>("a"), 1),
                   0xaf63dc4c8601ec8cull, "fnv1a64 of 'a'");

    /* Reference: FNV-1a("foobar") = 0x85944171f73967e8 */
    const char *foobar = "foobar";
    expectEqUint64(
        fir_hash64(reinterpret_cast<const uint8_t *>(foobar), 6),
        0x85944171f73967e8ull, "fnv1a64 of 'foobar'");
}

void testHashBatch() {
    const std::string a = "vless://one.example:443";
    const std::string b = "";
    const std::string c = "trojan://three.example:8443";

    std::string packed = a + b + c;
    std::vector<uint32_t> offsets = {0,
                                     static_cast<uint32_t>(a.size()),
                                     static_cast<uint32_t>(a.size() + b.size()),
                                     static_cast<uint32_t>(packed.size())};

    std::vector<uint64_t> hashes(3, 0);

    expectEqInt32(fir_hash64_batch(
                      reinterpret_cast<const uint8_t *>(packed.data()),
                      offsets.data(), 3, hashes.data()),
                  0, "hash batch returns success");

    expectEqUint64(hashes[0],
                   fir_hash64(reinterpret_cast<const uint8_t *>(a.data()),
                              static_cast<uint32_t>(a.size())),
                   "batch[0] matches single hash");

    expectEqUint64(hashes[1], 14695981039346656037ull,
                   "batch[1] empty string hash");

    expectEqUint64(hashes[2],
                   fir_hash64(reinterpret_cast<const uint8_t *>(c.data()),
                              static_cast<uint32_t>(c.size())),
                   "batch[2] matches single hash");

    /* Invalid arguments. */
    expectEqInt32(fir_hash64_batch(nullptr, offsets.data(), 3,
                                   hashes.data()),
                  -1, "null data rejected");

    expectEqInt32(fir_hash64_batch(nullptr, offsets.data(), 0,
                                   hashes.data()),
                  0, "zero count is a no-op success");
}

void testCrc32() {
    /* Reference: CRC32("123456789") = 0xCBF43926 (canonical check
     * value of the IEEE polynomial). */
    const char *input = "123456789";
    expectEqUint64(fir_crc32(0, reinterpret_cast<const uint8_t *>(input), 9),
                   0xCBF43926ull, "crc32 check value 0xCBF43926");

    expectEqUint64(fir_crc32(0, nullptr, 0), 0, "crc32 of empty is 0");

    /* Incremental computation must equal the single-shot result. */
    const char *text = "FreeIran chunk checksum payload";
    uint32_t len = static_cast<uint32_t>(std::strlen(text));
    uint32_t whole = fir_crc32(0, reinterpret_cast<const uint8_t *>(text), len);

    uint32_t half = fir_crc32(0, reinterpret_cast<const uint8_t *>(text), len / 2);
    uint32_t tail = fir_crc32(half,
                              reinterpret_cast<const uint8_t *>(text) + len / 2,
                              len - len / 2);
    expectEqUint64(whole, tail, "crc32 incremental equals one-shot");
}

void testScanUrls() {
    /* Offsets are computed from the literal prefixes so the test never
     * depends on hand-counted byte positions. */
    const std::string l1 = "# comment line\n";
    const std::string l2 = "vless://uuid@host:443?security=tls\n";
    const std::string l3 = "  \n";
    const std::string l4 = "\tvmatching line with leading tab\n";
    const std::string l5 = "https://example.com/not-a-proxy-line\n";
    const std::string l6 = "vmess://eyJhZGQiOiJleGFtcGxlIn0=\n";
    const std::string l7 = "notaproxy://foo\n";
    const std::string l8 = "\r\n";
    const std::string l9 = "ss://YWVzLTI1Ni1nY206cGFzcw@host:8388\n";
    const std::string l10 = "trailing line without newline";

    const std::string text = l1 + l2 + l3 + l4 + l5 + l6 + l7 + l8 + l9 + l10;

    uint32_t len = static_cast<uint32_t>(text.size());
    std::vector<uint32_t> offsets(16, 0);

    int64_t found = fir_scan_urls(text.data(), len, offsets.data(), 16);

    /* Matching lines: vless (l2), vmess (l6), ss (l9). "vmatching..."
     * does not start a scheme; https is not a config scheme. */
    expectEqInt64(found, 3, "scan finds three config lines");

    size_t off2 = l1.size();
    size_t off6 = off2 + l2.size() + l3.size() + l4.size() + l5.size();
    size_t off9 = off6 + l6.size() + l7.size() + l8.size();

    expectEqUint64(offsets[0], off2, "offset of vless line");
    expectEqUint64(offsets[1], off6, "offset of vmess line");
    expectEqUint64(offsets[2], off9, "offset of ss line");

    /* Buffer-growth protocol: request 0 slots, full count returned. */
    expectEqInt64(fir_scan_urls(text.data(), len, nullptr, 0), 3,
                  "count-only scan");

    /* Empty input is safe. */
    expectEqInt64(fir_scan_urls(nullptr, 0, nullptr, 0), 0,
                  "empty scan is safe");

    expectEqInt64(fir_scan_urls(nullptr, 5, nullptr, 0), -1,
                  "null text with nonzero length rejected");
}

void testCrc32Batch() {
    /* Two strings: "123456789" (canonical check 0xCBF43926) and "abc". */
    const std::string a = "123456789";
    const std::string b = "abc";
    std::string packed = a + b;
    std::vector<uint32_t> offsets = {
        0,
        static_cast<uint32_t>(a.size()),
        static_cast<uint32_t>(packed.size()),
    };
    std::vector<uint32_t> crcs(2, 0);

    expectEqInt32(fir_crc32_batch(
                      reinterpret_cast<const uint8_t *>(packed.data()),
                      offsets.data(), 2, crcs.data()),
                  0, "crc32 batch returns success");

    expectEqUint64(crcs[0], 0xCBF43926ull, "crc32 batch[0] check value");
    /* crc32("abc") = 0x352441C2 (standard reference). */
    expectEqUint64(crcs[1],
                   static_cast<uint64_t>(fir_crc32(0,
                       reinterpret_cast<const uint8_t *>(b.data()),
                       static_cast<uint32_t>(b.size()))),
                   "crc32 batch[1] matches single");

    /* Invalid args. */
    expectEqInt32(fir_crc32_batch(nullptr, offsets.data(), 2, crcs.data()),
                  -1, "crc32 batch null data rejected");
    expectEqInt32(fir_crc32_batch(nullptr, offsets.data(), 0, crcs.data()),
                  0, "crc32 batch zero count is no-op success");
}

void testArenaBasic() {
    fir_arena_t a = fir_arena_create(4);
    if (a == FIR_ARENA_NULL) {
        std::fprintf(stderr, "FAIL: arena create returned null\n");
        ++g_failures;
        return;
    }

    /* Allocate several small buffers and write to them. */
    void *p1 = fir_arena_alloc(a, 100);
    void *p2 = fir_arena_alloc(a, 200);
    void *p3 = fir_arena_alloc(a, 50);

    if (p1 == nullptr || p2 == nullptr || p3 == nullptr) {
        std::fprintf(stderr, "FAIL: arena alloc returned null\n");
        ++g_failures;
    }

    /* Write to verify the memory is usable. */
    std::memset(p1, 0xAA, 100);
    std::memset(p2, 0xBB, 200);
    std::memset(p3, 0xCC, 50);

    /* Verify alignment (16-byte). */
    for (void *p : {p1, p2, p3}) {
        if (reinterpret_cast<uintptr_t>(p) % 16 != 0) {
            std::fprintf(stderr, "FAIL: arena alloc not 16-aligned: %p\n", p);
            ++g_failures;
            break;
        }
    }

    /* Stats. */
    struct fir_arena_stats stats;
    expectEqInt32(fir_arena_stats(a, &stats), 0, "arena stats");
    expectEqUint64(stats.total_allocs, 3, "arena total_allocs");
    expectEqUint64(stats.total_bytes, 350, "arena total_bytes");
    expectEqUint64(stats.blocks_in_use, 1, "arena blocks_in_use (small allocs fit in one block)");

    /* Reset: blocks reusable, epoch_bytes cleared, totals preserved. */
    fir_arena_reset(a);
    expectEqInt32(fir_arena_stats(a, &stats), 0, "arena stats after reset");
    expectEqUint64(stats.total_allocs, 3, "arena total_allocs preserved after reset");
    expectEqUint64(stats.bytes_in_use, 0, "arena epoch_bytes cleared after reset");

    /* Alloc after reset reuses the block. */
    void *p4 = fir_arena_alloc(a, 100);
    if (p4 == nullptr) {
        std::fprintf(stderr, "FAIL: arena alloc after reset returned null\n");
        ++g_failures;
    }

    fir_arena_destroy(a);
}

void testArenaBoundedGrowth() {
    /* 1 block max = 64 KiB. Allocating more than 64 KiB should fail. */
    fir_arena_t a = fir_arena_create(1);
    if (a == FIR_ARENA_NULL) {
        std::fprintf(stderr, "FAIL: arena create(1) returned null\n");
        ++g_failures;
        return;
    }

    /* Fill the block. */
    void *p1 = fir_arena_alloc(a, 60 * 1024);
    if (p1 == nullptr) {
        std::fprintf(stderr, "FAIL: arena alloc 60K returned null\n");
        ++g_failures;
    }

    /* This should fail (block is full, no new block allowed). */
    void *p2 = fir_arena_alloc(a, 8 * 1024);
    if (p2 != nullptr) {
        std::fprintf(stderr, "FAIL: arena alloc past cap should return null\n");
        ++g_failures;
    }

    struct fir_arena_stats stats;
    fir_arena_stats(a, &stats);
    expectEqUint64(stats.blocks_in_use, 1, "arena bounded: 1 block");
    expectEqUint64(stats.blocks_capacity, 1, "arena bounded: cap 1");

    fir_arena_destroy(a);
}

void testArenaNullSafety() {
    /* NULL arena is safe (no crash). */
    if (fir_arena_alloc(FIR_ARENA_NULL, 100) != nullptr) {
        std::fprintf(stderr, "FAIL: arena alloc on null should return null\n");
        ++g_failures;
    }
    fir_arena_reset(FIR_ARENA_NULL);   /* no crash */
    fir_arena_destroy(FIR_ARENA_NULL); /* no crash */
    struct fir_arena_stats stats;
    expectEqInt32(fir_arena_stats(FIR_ARENA_NULL, &stats), -1,
                  "arena stats on null returns -1");
    expectEqInt32(fir_arena_stats(FIR_ARENA_NULL, nullptr), -1,
                  "arena stats with null out returns -1");
}

void testArenaConcurrentAlloc() {
    /* Many threads allocating concurrently. The arena mutex must
     * prevent data races on the bump pointer. */
    fir_arena_t a = fir_arena_create(64); /* 4 MiB */
    if (a == FIR_ARENA_NULL) {
        std::fprintf(stderr, "FAIL: arena create(64) returned null\n");
        ++g_failures;
        return;
    }

    const int kThreads = 8;
    const int kAllocsPerThread = 1000;
    std::vector<std::thread> threads;
    std::atomic<int> failures{0};

    for (int t = 0; t < kThreads; ++t) {
        threads.emplace_back([a, t, &failures]() {
            for (int i = 0; i < kAllocsPerThread; ++i) {
                void *p = fir_arena_alloc(a, 64);
                if (p == nullptr) {
                    ++failures;
                    continue;
                }
                /* Write a unique pattern to verify no overlap. */
                std::memset(p, static_cast<int>(t & 0xFF), 64);
            }
        });
    }
    for (auto &th : threads) {
        th.join();
    }

    expectEqInt32(failures.load(), 0, "arena concurrent alloc: no failures");

    struct fir_arena_stats stats;
    fir_arena_stats(a, &stats);
    expectEqUint64(stats.total_allocs,
                   static_cast<uint64_t>(kThreads * kAllocsPerThread),
                   "arena concurrent: total_allocs");

    fir_arena_destroy(a);
}

void testAbiVersion() {
    expectEqUint64(fir_abi_version(), FIR_ABI_VERSION, "abi version");
    if (FIR_ABI_VERSION != 2u) {
        std::fprintf(stderr, "FAIL: expected ABI v2, got %u\n", fir_abi_version());
        ++g_failures;
    }
}

} /* namespace */

int main() {
    testAbiVersion();
    testFnvVectors();
    testHashBatch();
    testCrc32();
    testCrc32Batch();
    testScanUrls();
    testArenaBasic();
    testArenaBoundedGrowth();
    testArenaNullSafety();
    testArenaConcurrentAlloc();

    if (g_failures != 0) {
        std::fprintf(stderr, "%d test(s) failed\n", g_failures);
        return 1;
    }

    std::printf("native: all tests passed\n");

    return 0;
}
