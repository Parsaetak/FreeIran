package shadowsocks

import "fmt"

// Method names the AEAD cipher suite of a Shadowsocks endpoint. Only the
// AEAD methods of the current specification (shadowsocks/shadowsocks-specs,
// "AEAD ciphers") are implemented: legacy stream ciphers are obsolete and
// insecure, and the AEAD-2022 edition is a different protocol.
type Method string

const (
	// MethodAES128GCM uses a 16-byte key and 16-byte salt.
	MethodAES128GCM Method = "aes-128-gcm"
	// MethodAES256GCM uses a 32-byte key and 32-byte salt.
	MethodAES256GCM Method = "aes-256-gcm"
	// MethodChaCha20IETFPoly1305 uses a 32-byte key and 32-byte salt.
	MethodChaCha20IETFPoly1305 Method = "chacha20-ietf-poly1305"
)

// ParseMethod resolves a configuration string to a Method. It accepts
// exactly the three supported AEAD method names and errors otherwise.
func ParseMethod(m string) (Method, error) {
	switch Method(m) {
	case MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305:
		return Method(m), nil
	default:
		return "", fmt.Errorf("shadowsocks: unsupported method %q (supported: %s, %s, %s)",
			m, MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305)
	}
}

// Supported reports whether m is one of the implemented AEAD methods.
func (m Method) Supported() bool {
	switch m {
	case MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305:
		return true
	default:
		return false
	}
}

// KeySize returns the master-key length in bytes; 0 for unsupported
// methods (unsupported methods never reach the crypto layer — they are
// rejected by validation first).
func (m Method) KeySize() int {
	switch m {
	case MethodAES128GCM:
		return 16
	case MethodAES256GCM, MethodChaCha20IETFPoly1305:
		return 32
	default:
		return 0
	}
}

// SaltSize returns the per-connection salt length in bytes; 0 for
// unsupported methods.
func (m Method) SaltSize() int {
	switch m {
	case MethodAES128GCM:
		return 16
	case MethodAES256GCM, MethodChaCha20IETFPoly1305:
		return 32
	default:
		return 0
	}
}

// TagSize returns the AEAD authentication-tag length in bytes; 0 for
// unsupported methods. All supported methods use 16-byte tags.
func (m Method) TagSize() int {
	if !m.Supported() {
		return 0
	}
	return tagSize
}
