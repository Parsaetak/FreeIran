package tun

import (
	"testing"
)

func TestDeriveGUIDDeterministic(t *testing.T) {
	a := DeriveGUID(DefaultSeed)
	b := DeriveGUID(DefaultSeed)

	if a != b {
		t.Fatalf("identical seeds produced different GUIDs")
	}

	c := DeriveGUID("another seed")
	if a == c {
		t.Fatalf("different seeds produced identical GUIDs")
	}
}

func TestDeriveGUIDShape(t *testing.T) {
	g := DeriveGUID(DefaultSeed)

	if version := g[6] >> 4; version != 5 {
		t.Fatalf("GUID version = %d, want 5 (RFC 4122 name-based)", version)
	}

	if variant := g[8] >> 6; variant != 2 {
		t.Fatalf("GUID variant = %d, want 2 (RFC 4122)", variant)
	}
}

func TestDefaultIdentityOwnership(t *testing.T) {
	id := DefaultIdentity()

	if !id.OwnsName("FreeIran") {
		t.Fatal("identity must own its exact adapter name")
	}

	if id.OwnsName("FreeIran ") || id.OwnsName("freeiran") || id.OwnsName("FreeIran 2") {
		t.Fatal("identity must NOT adopt fuzzy/foreign adapter names")
	}

	if !id.OwnsGUID(id.GUIDBytes) {
		t.Fatal("identity must own its exact GUID")
	}

	var foreign [16]byte
	foreign[0] = 0xAA
	if id.OwnsGUID(foreign) {
		t.Fatal("identity must not adopt a foreign GUID")
	}
}

func TestGUIDStringShape(t *testing.T) {
	s := DefaultIdentity().GUIDString()

	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		t.Fatalf("GUID string not in canonical form: %q", s)
	}

	if DefaultIdentity().ownershipFingerprint() == "" {
		t.Fatal("ownership fingerprint must be non-empty")
	}
}
