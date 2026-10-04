//go:build windows

package freecore

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"

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
// The index comes from CURRENT OS observation (tun.Observe) on every
// activation — never a hardcoded gateway, adapter name or interface
// number.
func init() {
	upstreamBinding = windowsUpstreamBinding
}

// unicastIfOption is IP_UNICAST_IF (v4) and IPV6_UNICAST_IF (v6) —
// both option number 31 in their respective level.
const unicastIfOption = 31

func windowsUpstreamBinding(interfaceIndex int) func(network, address string) (net.Conn, error) {
	if interfaceIndex <= 0 {
		return nil
	}

	return func(network, address string) (net.Conn, error) {
		dialer := &net.Dialer{
			Timeout:   DefaultDialTimeout,
			KeepAlive: 30 * 1e9,
			Control: func(_, addr string, c syscall.RawConn) error {
				var ctrlErr error

				err := c.Control(func(fd uintptr) {
					// IP_UNICAST_IF expects the interface index in
					// NETWORK byte order as a 4-byte option value.
					var idxBytes [4]byte

					binary.BigEndian.PutUint32(idxBytes[:], uint32(interfaceIndex))

					level := windows.IPPROTO_IP
					if isIPv6Literal(addr) {
						level = windows.IPPROTO_IPV6
					}

					ctrlErr = windows.SetsockoptInt(windows.Handle(fd), level, unicastIfOption,
						int(binary.BigEndian.Uint32(idxBytes[:])))
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

		conn, err := dialer.Dial(network, address)
		if err != nil {
			return nil, fmt.Errorf("freecore: constrained dial %s (interface %d): %w", address, interfaceIndex, err)
		}

		return conn, nil
	}
}

// isIPv6Literal reports whether the host part of addr is an IPv6
// literal (the option level follows the address family, not the
// network string).
func isIPv6Literal(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.To4() == nil
}
