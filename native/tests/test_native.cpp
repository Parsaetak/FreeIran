/*
 * FreeIran native layer unit tests.
 *
 * A deliberately dependency-free harness: failures print to stderr and
 * the process exits non-zero. Build & run:  make -C native test
 */
#include "../include/freeiran.h"

#include <cstdio>
#include <cstring>
#include <string>
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

void testAbiVersion() {
    expectEqUint64(fir_abi_version(), FIR_ABI_VERSION, "abi version");
}

} /* namespace */

int main() {
    testAbiVersion();
    testFnvVectors();
    testHashBatch();
    testCrc32();
    testScanUrls();

    if (g_failures != 0) {
        std::fprintf(stderr, "%d test(s) failed\n", g_failures);
        return 1;
    }

    std::printf("native: all tests passed\n");

    return 0;
}
