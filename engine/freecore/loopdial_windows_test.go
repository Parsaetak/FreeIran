//go:build windows

package freecore

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/windows"
)

// TestUnicastSocketOptionIPv4Literal pins the IPv4 shape: IP_UNICAST_IF
// at the IPPROTO_IP level, with the interface index encoded in NETWORK
// byte order — the four bytes the socket layer reads must be the
// big-endian encoding of the index (i.e. the host value byte-swapped).
func TestUnicastSocketOptionIPv4Literal(t *testing.T) {
	level, opt, value, err := unicastSocketOption(17, "tcp", "192.0.2.7:443")
	if err != nil {
		t.Fatalf("IPv4 literal: %v", err)
	}

	if level != windows.IPPROTO_IP {
		t.Fatalf("level = %d, want IPPROTO_IP (%d)", level, windows.IPPROTO_IP)
	}

	if opt != unicastIfOption {
		t.Fatalf("option = %d, want %d", opt, unicastIfOption)
	}

	// The in-memory representation of `value` (little-endian) must be
	// the big-endian encoding of 17 → [0,0,0,17].
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(value))

	if got := binary.BigEndian.Uint32(buf[:]); got != 17 {
		t.Fatalf("network-order representation = %d, want 17", got)
	}
}

// TestUnicastSocketOptionIPv6Literal pins the IPv6 shape:
// IPV6_UNICAST_IF at the IPPROTO_IPV6 level with the NATIVE interface
// index — a DIFFERENT encoding from the v4 option.
func TestUnicastSocketOptionIPv6Literal(t *testing.T) {
	level, opt, value, err := unicastSocketOption(17, "tcp", "[2001:db8::7]:443")
	if err != nil {
		t.Fatalf("IPv6 literal: %v", err)
	}

	if level != windows.IPPROTO_IPV6 {
		t.Fatalf("level = %d, want IPPROTO_IPV6 (%d)", level, windows.IPPROTO_IPV6)
	}

	if opt != unicastIfOption {
		t.Fatalf("option = %d, want %d", opt, unicastIfOption)
	}

	if value != 17 {
		t.Fatalf("native index value = %d, want 17", value)
	}
}

// TestUnicastSocketOptionHostnameIsFamilyAgnostic pins the rule that a
// HOSTNAME must not be assumed to be IPv4 from its text: with no
// literal available, an ambiguous "tcp" fails closed instead of
// guessing the v4 option for a name that may resolve to v6.
func TestUnicastSocketOptionHostnameAmbiguousRefused(t *testing.T) {
	if _, _, _, err := unicastSocketOption(17, "tcp", "proxy.example.invalid:1080"); err == nil {
		t.Fatal("ambiguous hostname dial accepted — family guessed (must fail closed)")
	}
}

// TestEffectiveFamily covers the family decision table: resolved
// literals are authoritative (regardless of the network string), and
// only an EXPLICIT family in the network string decides when no
// literal exists.
func TestEffectiveFamily(t *testing.T) {
	cases := []struct {
		name    string
		network string
		address string
		want6   bool
		wantErr bool
	}{
		{name: "v4 literal on tcp", network: "tcp", address: "192.0.2.7:443", want6: false},
		{name: "v6 literal on tcp", network: "tcp", address: "[2001:db8::1]:443", want6: true},
		{name: "v4-mapped literal is v4", network: "tcp", address: "::ffff:192.0.2.7:443", want6: false},
		{name: "explicit tcp4 wins over nothing", network: "tcp4", address: "proxy.example.invalid:1080", want6: false},
		{name: "explicit tcp6 wins over nothing", network: "tcp6", address: "proxy.example.invalid:1080", want6: true},
		{name: "literal overrides tcp6 text", network: "tcp6", address: "192.0.2.7:443", want6: false},
		{name: "literal overrides tcp4 text", network: "tcp4", address: "[2001:db8::1]:443", want6: true},
		{name: "ambiguous hostname refused", network: "tcp", address: "proxy.example.invalid:1080", wantErr: true},
		{name: "malformed address refused", network: "tcp", address: "no-port", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			is6, err := effectiveFamily(tc.network, tc.address)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("effectiveFamily(%q, %q) = %v, want error", tc.network, tc.address, is6)
				}

				return
			}

			if err != nil {
				t.Fatalf("effectiveFamily(%q, %q): %v", tc.network, tc.address, err)
			}

			if is6 != tc.want6 {
				t.Fatalf("effectiveFamily(%q, %q) = v6=%v, want v6=%v", tc.network, tc.address, is6, tc.want6)
			}
		})
	}
}

// TestUnicastSocketOptionZeroIndexRefused: index 0 is "unspecified" —
// binding to it would be an unbound socket (loop risk).
func TestUnicastSocketOptionZeroIndexRefused(t *testing.T) {
	if _, _, _, err := unicastSocketOption(0, "tcp", "192.0.2.7:443"); err == nil {
		t.Fatal("interface index 0 accepted (must refuse: unbound socket)")
	}
}

// TestWindowsUpstreamBindingNilOnZeroIndex: the binding factory refuses
// to produce a binding for a zero/unknown interface.
func TestWindowsUpstreamBindingNilOnZeroIndex(t *testing.T) {
	if windowsUpstreamBinding(0) != nil {
		t.Fatal("binding produced for interface index 0")
	}

	if windowsUpstreamBinding(-1) != nil {
		t.Fatal("binding produced for negative interface index")
	}
}
