package tun

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
)

// Identity is the deterministic FreeIran-owned adapter identity. The
// Phase 2 dataplane (and every observation/rollback decision) keys
// ownership on EXACTLY these values — never on "the first adapter we
// found" — so a stale or foreign adapter can never be mistaken for
// FreeIran's and FreeIran's can never be mistaken for foreign state.
type Identity struct {
	// AdapterName is the Wintun adapter name FreeIran creates.
	AdapterName string

	// TunnelType is the Wintun tunnel type (the driver-side service
	// name the adapter is grouped under).
	TunnelType string

	// GUIDBytes is the deterministic adapter GUID (RFC 4122 version 5
	// shape, derived from a fixed seed). Requesting a fixed GUID at
	// creation makes the identity reproducible across sessions: the
	// same FreeIran installation always owns the same adapter slot.
	GUIDBytes [16]byte
}

// DefaultSeed is the fixed derivation seed for the default identity.
// Changing it deliberately changes the adapter slot on every
// installation — a migration, not a bug fix.
const DefaultSeed = "FreeIran first-party TUN adapter v1"

// DefaultIdentity returns the deterministic FreeIran adapter
// identity.
func DefaultIdentity() Identity {
	return Identity{
		AdapterName: "FreeIran",
		TunnelType:  "FreeIran",
		GUIDBytes:   DeriveGUID(DefaultSeed),
	}
}

// DeriveGUID deterministically derives a 16-byte GUID (RFC 4122
// version 5 shape: SHA-1 of the seed, variant/version bits set) from
// a seed string. Identical seeds always produce identical GUIDs —
// the property adapter-identity reproducibility needs.
func DeriveGUID(seed string) [16]byte {
	sum := sha1.Sum([]byte("freecore:tun:" + seed))

	var guid [16]byte

	copy(guid[:], sum[:16])

	// version 5 (name-based, SHA-1) in the high nibble of byte 6
	guid[6] = (guid[6] & 0x0f) | 0x50

	// RFC 4122 variant in the high bits of byte 8
	guid[8] = (guid[8] & 0x3f) | 0x80

	return guid
}

// GUIDString renders the GUID in the canonical registry form.
func (i Identity) GUIDString() string {
	b := i.GUIDBytes

	return hex.EncodeToString(b[0:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}

// OwnsName reports whether an observed adapter name is FreeIran's.
// The comparison is exact: deterministic identity, no prefix or
// fuzzy matching — a similarly named foreign adapter must never be
// adopted (and FreeIran's must never be touched as if foreign).
func (i Identity) OwnsName(name string) bool {
	return name == i.AdapterName
}

// OwnsGUID reports whether observed GUID bytes are FreeIran's.
func (i Identity) OwnsGUID(guid [16]byte) bool {
	return guid == i.GUIDBytes
}

// ownershipFingerprint is a stable, credential-free identity digest
// for logs and session markers.
func (i Identity) ownershipFingerprint() string {
	b := make([]byte, 8)

	binary.BigEndian.PutUint64(b, uint64(i.GUIDBytes[0])<<56|
		uint64(i.GUIDBytes[1])<<48|
		uint64(i.GUIDBytes[2])<<40|
		uint64(i.GUIDBytes[3])<<32|
		uint64(i.GUIDBytes[4])<<24|
		uint64(i.GUIDBytes[5])<<16|
		uint64(i.GUIDBytes[6])<<8|
		uint64(i.GUIDBytes[7]))

	return hex.EncodeToString(b)
}
