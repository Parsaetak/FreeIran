// Package shadowsocks implements the Shadowsocks AEAD TCP protocol as
// specified by shadowsocks/shadowsocks-specs ("AEAD ciphers", the current
// protocol specification — not the legacy stream-cipher design).
//
// Scope is deliberately minimal and complete:
//
//   - AEAD methods only: aes-128-gcm, aes-256-gcm, chacha20-ietf-poly1305
//     (16-byte tag; no legacy stream ciphers, no AEAD-2022, no plugins).
//   - Key derivation: master key via OpenSSL EVP_BytesToKey (MD5, count=1,
//     no salt) and per-connection subkey via HKDF-SHA1 with the info
//     string "ss-subkey".
//   - TCP framing: [salt][chunk]*, chunk = [encrypted 2-byte big-endian
//     length + tag][encrypted payload + tag], payload limit 0x3FFF, a
//     zero-length chunk decoded as end-of-stream, 12-byte little-endian
//     incrementing nonce (one counter per direction, one increment per
//     AEAD operation).
//   - The first payload chunk carries the target address in SOCKS5 ATYP
//     form (IPv4 / domain / IPv6 + big-endian port).
//
// Evidence gating: this package is gated on interop evidence. It may only
// be considered release-ready when its tests demonstrate byte-level
// interoperability with an independent, spec-compliant reference
// implementation (github.com/shadowsocks/go-shadowsocks2) in BOTH
// directions — our client through their server, and their client through
// our server — for every supported method. The interop test
// (interop_test.go) encodes that rung of the evidence ladder; if the
// reference module is unavailable in a build environment, the test
// degrades to an independent spec-driven fixture and reports the reduced
// evidence level honestly instead of silently passing.
//
// Security posture of the framing layer: every authentication failure
// fails the connection closed (the conn is closed and a real error is
// returned — never a silent EOF), chunk lengths above the spec limit are
// rejected, all reads are bounded (fixed-size chunk buffers), and no
// payload bytes are ever logged.
//
// The only external cryptographic dependency is golang.org/x/crypto for
// the ChaCha20-IETF-Poly1305 primitive (already required transitively by
// the module graph); AES-GCM comes from the standard library and the KDF
// primitives (EVP_BytesToKey, HKDF-SHA1) are first-party, cross-checked
// against RFC vectors and x/crypto in the test suite.
package shadowsocks
