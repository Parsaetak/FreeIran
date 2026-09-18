//go:build unix

// traceroute_unix.go — TTL control for raw ICMP sockets on Unix
// platforms (Linux, macOS). Raw sockets still require elevated
// privileges to open; the traceroute tool reports an honest
// "unsupported" status when the socket cannot be created.
package netcheck

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// setPacketTTL sets the IP TTL used for subsequent writes.
func setPacketTTL(conn net.PacketConn, ttl int) error {
	raw, ok := conn.(syscall.Conn)
	if !ok {
		return errors.New("socket does not support TTL control")
	}

	control, err := raw.SyscallConn()
	if err != nil {
		return err
	}

	var setErr error

	err = control.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TTL, ttl)
	})
	if err != nil {
		return err
	}

	return setErr
}

// setICMPFilterAll is best-effort socket shaping (no-op where the
// platform lacks the filter; ID matching discards foreign packets).
func setICMPFilterAll(conn net.PacketConn) error {
	return nil
}
