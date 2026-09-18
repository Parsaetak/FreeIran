//go:build windows

// traceroute_windows.go — TTL control for raw ICMP sockets on
// Windows. Raw sockets require Administrator privileges; the
// traceroute tool reports an honest "unsupported" status when the
// socket cannot be created.
package netcheck

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
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
		setErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, windows.IP_TTL, ttl)
	})
	if err != nil {
		return err
	}

	return setErr
}

// setICMPFilterAll is best-effort socket shaping (no-op; ID matching
// discards foreign packets).
func setICMPFilterAll(conn net.PacketConn) error {
	return nil
}
