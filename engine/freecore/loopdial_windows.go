//go:build windows

package freecore

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// upstreamBinding (Phase D, Windows implementation) returns a dial
// function whose sockets are bound to the PHYSICAL interface index the
// loop-prevention policy selected — via IP_UNICAST_IF /
// IPV6_UNICAST_IF, the same socket option the major Windows tunnel
// clients use. The engine's upstream connections therefore CANNOT
// re-enter the FreeIran TUN while it owns traffic: the OS routes them
// out the physical interface regardless of the TUN's covered routes.
//
// v0.14.1 correctness contract (audited against the current reference
// behavior of Windows tunnel clients — not a source copy):
//
//   - IPv4: IP_UNICAST_IF (option 31, IPPROTO_IP level) takes the
//     interface index in NETWORK byte order — the four bytes setsockopt
//     receives must be the big-endian encoding of the index;
//   - IPv6: IPV6_UNICAST_IF (option 31, IPPROTO_IPV6 level) takes the
//     interface index in NATIVE (host) byte order;
//   - the address family comes from the RESOLVED destination address
//     (what Control actually receives) or an explicit "tcp4"/"tcp6"
//     network — never from guessing whether a hostname "looks like"
//     IPv4 or IPv6 text;
//   - the dial honors the caller's context (DialContext, never Dial),
//     so cancellation reaches the connect.
//
// The index comes from CURRENT OS observation (tun.Observe) on every
// activation — never a hardcoded gateway, adapter name or interface
// number.
func init() {
	upstreamBinding = windowsUpstreamBinding
}

// unicastIfOption is IP_UNICAST_IF (v4) and IPV6_UNICAST_IF (v6) —
// both option number 31 in their respective level.
const unicastIfOption = 31

// effectiveFamily decides the REAL socket family for one dial.
//
// Control receives the RESOLVED destination address, so an IP literal
// there is authoritative regardless of what the network string says
// (a "tcp" dial to a v6 literal, or to a hostname resolving to v6, is
// an IPv6 socket). Only when no literal is available may an EXPLICIT
// family in the network string decide; an ambiguous "tcp" with no
// literal is an error — fail closed beats guessing a family.
func effectiveFamily(network, address string) (isIPv6 bool, err error) {
	if host, _, splitErr := net.SplitHostPort(address); splitErr == nil {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")

		if ip := net.ParseIP(host); ip != nil {
			return ip.To4() == nil, nil
		}
	}

	switch network {
	case "tcp4", "udp4", "ip4":
		return false, nil
	case "tcp6", "udp6", "ip6":
		return true, nil
	}

	return false, fmt.Errorf("freecore: cannot determine address family for %q (network %q)", address, network)
}

// unicastSocketOption computes the (level, option, value) triple that
// binds one socket to the interface for one dialed destination.
//
// IPv4 returns the interface index encoded in NETWORK byte order: the
// value integer carries the index byte-swapped, so the four bytes the
// Windows socket layer reads are the big-endian encoding of the index.
// IPv6 returns the native interface index. These are two DIFFERENT
// encodings — the pre-0.14.1 code applied one treatment to both
// families (and inferred the family from hostname text, misbinding
// domain-resolved-v6 upstreams to the v4 option).
func unicastSocketOption(interfaceIndex uint32, network, address string) (level int, opt int, value int, err error) {
	if interfaceIndex == 0 {
		return 0, 0, 0, fmt.Errorf("freecore: upstream binding requires a physical interface index")
	}

	isIPv6, err := effectiveFamily(network, address)
	if err != nil {
		return 0, 0, 0, err
	}

	if isIPv6 {
		// IPV6_UNICAST_IF: native (host) interface index.
		return windows.IPPROTO_IPV6, unicastIfOption, int(interfaceIndex), nil
	}

	// IP_UNICAST_IF: the option value is the interface index in
	// network byte order. BigEndian encodes the index into bytes;
	// reading those bytes as a little-endian integer yields the
	// byte-swapped host value whose in-memory representation is the
	// big-endian encoding — exactly what the API requires.
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], interfaceIndex)

	return windows.IPPROTO_IP, unicastIfOption, int(binary.LittleEndian.Uint32(be[:])), nil
}

func windowsUpstreamBinding(interfaceIndex int) func(ctx context.Context, network, address string) (net.Conn, error) {
	if interfaceIndex <= 0 {
		return nil
	}

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		dialer := &net.Dialer{
			Timeout:   DefaultDialTimeout,
			KeepAlive: 30 * time.Second,
			Control: func(_, addr string, c syscall.RawConn) error {
				var ctrlErr error

				err := c.Control(func(fd uintptr) {
					level, opt, value, optErr := unicastSocketOption(uint32(interfaceIndex), network, addr)
					if optErr != nil {
						ctrlErr = optErr

						return
					}

					ctrlErr = windows.SetsockoptInt(windows.Handle(fd), level, opt, value)
				})
				if err != nil {
					return err
				}

				if ctrlErr != nil {
					return fmt.Errorf("freecore: bind upstream to interface %d: %w", interfaceIndex, ctrlErr)
				}

				return nil
			},
		}

		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, fmt.Errorf("freecore: constrained dial %s (interface %d): %w", address, interfaceIndex, err)
		}

		return conn, nil
	}
}
