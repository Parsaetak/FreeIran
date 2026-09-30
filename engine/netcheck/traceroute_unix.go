//go:build unix

// traceroute_unix.go — the Unix hop walker: a raw ip4:icmp socket
// with per-probe TTL control. Raw sockets require elevated privileges
// (root / CAP_NET_RAW); when the socket cannot be created the
// traceroute tool reports the honest "unsupported" status. The
// Windows counterpart (traceroute_windows.go) walks the path through
// the native IP Helper ICMP API instead — user-mode, no elevation.
package netcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// icmpSocketWalker is the Unix hopWalker implementation.
type icmpSocketWalker struct {
	conn net.PacketConn
}

// newHopWalker opens the platform ICMP walker. A refused raw socket
// (privilege, security policy, container limits) returns the error the
// tool surfaces as "unsupported".
func newHopWalker(ctx context.Context) (hopWalker, error) {
	_ = ctx // the walker is bounded per-probe and by the caller context

	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, err
	}

	// Best-effort socket shaping: without the filter we simply see
	// more noise, which the echo-ID matching already discards.
	_ = setICMPFilterAll(conn)

	return &icmpSocketWalker{conn: conn}, nil
}

// probe sends ONE echo request at the given TTL and waits the bounded
// window for either a time-exceeded from an intermediate hop or an
// echo reply from the target (probe retries are the walk loop's
// concern — walkTTLs in tools_path.go).
func (w *icmpSocketWalker) probe(ctx context.Context, target net.IP, echoID uint16, ttl int) (hopIP string, replied bool, err error) {
	return icmpSocketProbe(w.conn, ctx, target, echoID, ttl)
}

// Close releases the raw socket.
func (w *icmpSocketWalker) Close() error { return w.conn.Close() }

// icmpSocketProbe is the shared raw-socket probe: build the echo,
// set the TTL, send, and classify every incoming packet until the
// answer window closes.
func icmpSocketProbe(conn net.PacketConn, ctx context.Context, target net.IP, echoID uint16, ttl int) (hopIP string, replied bool, err error) {
	seq := uint16(ttl & 0xff)

	payload := []byte("freeiran-traceroute-probe")
	packet := buildICMPEcho(echoID, seq, payload)

	if err := setPacketTTL(conn, ttl); err != nil {
		return "", false, fmt.Errorf("set TTL %d: %w", ttl, err)
	}

	if _, err := conn.WriteTo(packet, &net.IPAddr{IP: target.To4()}); err != nil {
		return "", false, err
	}

	deadline := time.Now().Add(time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	buf := make([]byte, 1500)

	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)

		n, from, readErr := conn.ReadFrom(buf)
		if readErr != nil {
			break // probe answer window over
		}

		hop, replied, matchErr := parseICMPResponse(buf[:n], from, target, echoID)
		if matchErr != nil {
			continue // not ours
		}

		if replied {
			return hop, true, nil
		}

		if hop != "" {
			return hop, false, nil
		}
	}

	// Silent window: no answer — never fabricated.
	return "", false, nil
}

// setPacketTTL sets the IP TTL used for subsequent writes.
func setPacketTTL(conn net.PacketConn, ttl int) error {
	raw, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
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
