package shadowsocks

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"errors"
	"fmt"
)

// errEmptyPassword is returned by DeriveKey for empty passwords. The
// spec has no notion of an empty password; deriving a key from one would
// silently produce a known constant key, so it is refused.
var errEmptyPassword = errors.New("shadowsocks: password must not be empty")

// DeriveKey derives the master key for method from password using the
// OpenSSL EVP_BytesToKey KDF (MD5 digest, iteration count 1, no salt) —
// the exact derivation the AEAD specification defines for Shadowsocks
// passwords. The password must be non-empty and the method supported.
func DeriveKey(method Method, password string) ([]byte, error) {
	if !method.Supported() {
		return nil, fmt.Errorf("shadowsocks: cannot derive key for unsupported method %q", string(method))
	}
	if password == "" {
		return nil, errEmptyPassword
	}
	return evpBytesToKey([]byte(password), method.KeySize()), nil
}

// evpBytesToKey implements OpenSSL EVP_BytesToKey for the parameters
// Shadowsocks fixes: MD5 digest, count=1, iv=nil. It concatenates
// MD5(prev || password) blocks until keyLen bytes are available. Kept
// small and allocation-light because every connection setup runs it;
// correctness is pinned by kdf_test.go against an independent
// reimplementation and hard-coded vectors.
func evpBytesToKey(password []byte, keyLen int) []byte {
	out := make([]byte, 0, keyLen+md5.Size)
	var prev []byte
	for len(out) < keyLen {
		h := md5.New()
		h.Write(prev)
		h.Write(password)
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:keyLen]
}

// deriveSubkey derives the per-connection subkey from the master key and
// the salt exactly as the AEAD specification prescribes: HKDF-SHA1 with
// info = "ss-subkey" and output length equal to the key length. Both
// directions of a connection use the same subkey (derived from the peer's
// respective salt) with separate nonce counters.
func deriveSubkey(master, salt []byte, keyLen int) []byte {
	return hkdfSHA1(master, salt, []byte(subkeyInfo), keyLen)
}

// hkdfSHA1 implements RFC 5869 HKDF with HMAC-SHA1: Extract (PRK =
// HMAC-Hash(salt, IKM)) followed by Expand (T(i) = HMAC-Hash(PRK,
// T(i-1) || info || i)). A zero-length salt is replaced by HashLen zero
// octets per the RFC. First-party on purpose — the test suite proves it
// equivalent to golang.org/x/crypto/hkdf and to hand-computed RFC
// vectors, so the production code needs no HKDF dependency.
func hkdfSHA1(master, salt, info []byte, outLen int) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha1.Size)
	}

	// Extract: PRK = HMAC-SHA1(salt, IKM).
	mac := hmac.New(sha1.New, salt)
	mac.Write(master)
	prk := mac.Sum(nil)

	// Expand: OKM = T(1) || T(2) || ... truncated to outLen. The HMAC is
	// re-keyed to the PRK for this phase.
	mac = hmac.New(sha1.New, prk)
	okm := make([]byte, 0, outLen+sha1.Size)
	var prev []byte
	for counter := byte(1); len(okm) < outLen; counter++ {
		mac.Reset()
		mac.Write(prev)
		mac.Write(info)
		mac.Write([]byte{counter})
		prev = mac.Sum(prev[:0])
		okm = append(okm, prev...)
	}
	return okm[:outLen]
}
