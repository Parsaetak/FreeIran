package shadowsocks

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"

	"golang.org/x/crypto/hkdf"
)

// Hard-coded EVP_BytesToKey vectors for password "test" (MD5 digest,
// count=1, salt=nil). Computed FIRST with an independent scratch
// implementation written directly from the OpenSSL algorithm description
// and independently cross-checked against the OpenSSL CLI:
//
//	echo -n test | openssl enc -e -aes-128-cbc -k test -nosalt -md md5 -P
//	  key=098F6BCD4621D373CADE4E832627B4F6
//	echo -n test | openssl enc -e -aes-256-cbc -k test -nosalt -md md5 -P
//	  key=098F6BCD4621D373CADE4E832627B4F60A9172716AE6428409885B8B829CCB05
const (
	evpTestPassword16Hex = "098f6bcd4621d373cade4e832627b4f6"
	evpTestPassword32Hex = "098f6bcd4621d373cade4e832627b4f60a9172716ae6428409885b8b829ccb05"
)

// independentEVPBytesToKey is an inline INDEPENDENT reimplementation of
// OpenSSL EVP_BytesToKey for the parameters Shadowsocks fixes (MD5
// digest, count=1, salt=nil), written from the algorithm description
// D_i = MD5(D_{i-1} || password) with a hash-sum based structure that
// deliberately differs from the production code (which streams into a
// persistent hasher). Used to cross-check evpBytesToKey.
func independentEVPBytesToKey(password []byte, keyLen int) []byte {
	var out []byte
	var prev []byte
	for len(out) < keyLen {
		buf := make([]byte, 0, len(prev)+len(password))
		buf = append(buf, prev...)
		buf = append(buf, password...)
		sum := md5.Sum(buf)
		prev = sum[:]
		out = append(out, prev...)
	}
	return out[:keyLen]
}

// TestDeriveKeyHardcodedVectors pins DeriveKey to the externally
// computed (scratch implementation + OpenSSL CLI) vectors for the
// password "test" at both key sizes used by the supported methods.
func TestDeriveKeyHardcodedVectors(t *testing.T) {
	want16, err := hex.DecodeString(evpTestPassword16Hex)
	if err != nil {
		t.Fatalf("decode 16-byte vector: %v", err)
	}
	want32, err := hex.DecodeString(evpTestPassword32Hex)
	if err != nil {
		t.Fatalf("decode 32-byte vector: %v", err)
	}

	cases := []struct {
		method Method
		want   []byte
	}{
		{MethodAES128GCM, want16},
		{MethodAES256GCM, want32},
		{MethodChaCha20IETFPoly1305, want32},
	}
	for _, tc := range cases {
		t.Run(string(tc.method), func(t *testing.T) {
			got, err := DeriveKey(tc.method, "test")
			if err != nil {
				t.Fatalf("DeriveKey(%s, \"test\"): %v", tc.method, err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("DeriveKey(%s, \"test\") = %x, want %x", tc.method, got, tc.want)
			}
			if len(got) != tc.method.KeySize() {
				t.Fatalf("key length %d does not match KeySize() %d", len(got), tc.method.KeySize())
			}
		})
	}
}

// TestDeriveKeyAgainstIndependentImplementation cross-checks evpBytesToKey
// (via DeriveKey) against the independent inline reimplementation for a
// spread of passwords, including multi-block keys and non-ASCII bytes.
func TestDeriveKeyAgainstIndependentImplementation(t *testing.T) {
	passwords := []string{
		"test",
		"correct horse battery staple",
		"پارسی-λ-密码",                       // non-ASCII, exercises byte-level (not rune-level) handling
		"0123456789abcdef0123456789abcdef", // password longer than one MD5 block
		"x",
	}
	for _, method := range []Method{MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305} {
		for _, pw := range passwords {
			want := independentEVPBytesToKey([]byte(pw), method.KeySize())
			got, err := DeriveKey(method, pw)
			if err != nil {
				t.Fatalf("DeriveKey(%s, %q): %v", method, pw, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("DeriveKey(%s, %q) = %x, independent = %x", method, pw, got, want)
			}
		}
	}
}

// TestHKDFSubkeyAgainstXCrypto verifies deriveSubkey is byte-identical to
// golang.org/x/crypto/hkdf (HKDF-SHA1, info "ss-subkey") for random
// master keys and salts, including the zero-length salt edge case both
// implementations must normalize to HashLen zero octets (RFC 5869 §2.2).
func TestHKDFSubkeyAgainstXCrypto(t *testing.T) {
	sizes := map[Method]int{
		MethodAES128GCM:            16,
		MethodAES256GCM:            32,
		MethodChaCha20IETFPoly1305: 32,
	}

	for i := 0; i < 8; i++ {
		master := make([]byte, 32)
		if _, err := rand.Read(master); err != nil {
			t.Fatalf("read master: %v", err)
		}
		salt := make([]byte, 32)
		if _, err := rand.Read(salt); err != nil {
			t.Fatalf("read salt: %v", err)
		}
		// Exercise: random 32-byte salt, short salt, full-size-slice salt,
		// and the zero-length salt edge case.
		switch i % 4 {
		case 1:
			salt = salt[:16]
		case 2:
			salt = salt[:1]
		case 3:
			salt = salt[:0]
		}

		for method, keyLen := range sizes {
			want := make([]byte, keyLen)
			r := hkdf.New(sha1.New, master, salt, []byte(subkeyInfo))
			if _, err := io.ReadFull(r, want); err != nil {
				t.Fatalf("x/crypto/hkdf: %v", err)
			}
			got := deriveSubkey(master, salt, keyLen)
			if !bytes.Equal(got, want) {
				t.Fatalf("iter %d: deriveSubkey(%s, salt len %d) = %x, x/crypto = %x",
					i, method, len(salt), got, want)
			}
		}
	}
}

// TestDeriveKeyEmptyPasswordRefused: an empty password must be refused
// (it would silently derive a known constant key).
func TestDeriveKeyEmptyPasswordRefused(t *testing.T) {
	for _, method := range []Method{MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305} {
		key, err := DeriveKey(method, "")
		if err == nil {
			t.Fatalf("DeriveKey(%s, \"\") accepted an empty password", method)
		}
		if !errors.Is(err, errEmptyPassword) {
			t.Fatalf("DeriveKey(%s, \"\") error = %v, want errEmptyPassword chain", method, err)
		}
		if key != nil {
			t.Fatalf("DeriveKey(%s, \"\") returned a key on error", method)
		}
	}
}

// TestUnsupportedMethodRefused: unsupported method names must be refused
// by ParseMethod, Method.Supported, the sizing accessors, newAEAD and
// DeriveKey — the crypto layer never sees an unsupported method.
func TestUnsupportedMethodRefused(t *testing.T) {
	unsupported := []string{
		"",
		"aes-192-gcm",
		"AES-128-GCM", // case-sensitive: only the lowercase spec name is accepted
		"aes-128-gcm ",
		"rc4-md5",
		"aes-256-gcm-tcp",
		"2022-blake3-aes-256-gcm",
	}
	for _, name := range unsupported {
		m, err := ParseMethod(name)
		if err == nil {
			t.Fatalf("ParseMethod(%q) accepted an unsupported method", name)
		}
		if m != "" {
			t.Fatalf("ParseMethod(%q) returned a method on error", name)
		}
		method := Method(name)
		if method.Supported() {
			t.Fatalf("Method(%q).Supported() = true", name)
		}
		if got := method.KeySize(); got != 0 {
			t.Fatalf("Method(%q).KeySize() = %d, want 0", name, got)
		}
		if got := method.SaltSize(); got != 0 {
			t.Fatalf("Method(%q).SaltSize() = %d, want 0", name, got)
		}
		if got := method.TagSize(); got != 0 {
			t.Fatalf("Method(%q).TagSize() = %d, want 0", name, got)
		}
		if _, err := DeriveKey(method, "test"); err == nil {
			t.Fatalf("DeriveKey(%q, \"test\") accepted an unsupported method", name)
		}
	}

	// Supported methods must report the spec sizes.
	for _, tc := range []struct {
		method       Method
		key, saltTag int
	}{
		{MethodAES128GCM, 16, 16},
		{MethodAES256GCM, 32, 32},
		{MethodChaCha20IETFPoly1305, 32, 32},
	} {
		if _, err := ParseMethod(string(tc.method)); err != nil {
			t.Fatalf("ParseMethod(%q): %v", tc.method, err)
		}
		if !tc.method.Supported() {
			t.Fatalf("Method(%q).Supported() = false", tc.method)
		}
		if got := tc.method.KeySize(); got != tc.key {
			t.Fatalf("Method(%q).KeySize() = %d, want %d", tc.method, got, tc.key)
		}
		if got := tc.method.SaltSize(); got != tc.saltTag {
			t.Fatalf("Method(%q).SaltSize() = %d, want %d", tc.method, got, tc.saltTag)
		}
		if got := tc.method.TagSize(); got != 16 {
			t.Fatalf("Method(%q).TagSize() = %d, want 16", tc.method, got)
		}
	}
}

// TestHKDFSubkeyDeterministic pins a fully hand-computed HKDF-SHA1 vector
// (RFC 5869 style: computed from the independent scratch implementation
// with the same constants as the production path) so a future refactor
// cannot silently change the subkey derivation.
func TestHKDFSubkeyDeterministic(t *testing.T) {
	// master = SHA-1("freeiran-master"), salt = SHA-1("freeiran-salt")
	master := sha1.Sum([]byte("freeiran-master"))
	salt := sha1.Sum([]byte("freeiran-salt"))

	// Expected value computed with the independent scratch program
	// (RFC 5869 HKDF-SHA1: PRK = HMAC-SHA1(salt, master); OKM via Expand
	// with info "ss-subkey", 16 bytes), verified against x/crypto/hkdf.
	const want16Hex = "0a63948a86a42868d4753b1f76c62ee8"
	want, err := hex.DecodeString(want16Hex)
	if err != nil {
		t.Fatal(err)
	}
	got := deriveSubkey(master[:], salt[:], 16)
	if !bytes.Equal(got, want) {
		t.Fatalf("deterministic subkey = %x, want %x", got, want)
	}

	// Cross-check the same vector against x/crypto/hkdf in-test so the
	// constant cannot drift from the library.
	r := hkdf.New(sha1.New, master[:], salt[:], []byte(subkeyInfo))
	lib := make([]byte, 16)
	if _, err := io.ReadFull(r, lib); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, lib) {
		t.Fatalf("hard-coded vector %x disagrees with x/crypto/hkdf %x", want, lib)
	}
}

// TestNewAEADUnsupportedRefused pins that the AEAD factory refuses
// unsupported methods instead of panicking.
func TestNewAEADUnsupportedRefused(t *testing.T) {
	for _, method := range []Method{"", "aes-192-gcm", "rc4-md5"} {
		_, err := newAEAD(method, make([]byte, 32))
		if err == nil {
			t.Fatalf("newAEAD(%q) accepted an unsupported method", method)
		}
		want := fmt.Sprintf("unsupported method %q", method)
		if !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Fatalf("newAEAD(%q) error = %v, want it to contain %q", method, err, want)
		}
	}
}
