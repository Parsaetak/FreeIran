package freecore

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"time"
)

// SOCKS5Inbound serves the local SOCKS5 surface (RFC 1928 subset):
// no-auth CONNECT only. BIND and UDP ASSOCIATE are refused with the
// protocol's command-not-supported reply — the engine never fakes a
// capability it does not implement.
type SOCKS5Inbound struct {
	// Engine is the serving engine (session registry + pipeline).
	Engine *Engine
}

// Name identifies the inbound in session metadata.
func (s *SOCKS5Inbound) Name() string { return "socks5" }

// Serve handles one accepted connection (conn must be a *prefixConn
// whose sniff byte has already been consumed when mixed-port
// dispatching is in play; plain conns work too).
func (s *SOCKS5Inbound) Serve(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	if !s.greet(conn) {
		return
	}

	target, ok := s.readConnect(conn)
	if !ok {
		return
	}

	s.Engine.pipe(ctx, conn, s.Name(), target,
		func(upstreamLocal net.Addr) error {
			_, err := conn.Write(socksSuccessReply(upstreamLocal))

			return err
		},
		func(error) {
			// The outbound refused the target: the honest generic
			// reply is general SOCKS server failure (0x01).
			s.reply(conn, 0x01)
		},
	)
}

// sniffSOCKS5 reports whether the first byte of a connection is a
// SOCKS5 greeting (version 5) — used by the mixed inbound to
// distinguish SOCKS5 from HTTP on one port.
func sniffSOCKS5(first byte) bool { return first == 0x05 }

// greet reads the method negotiation and replies no-auth.
func (s *SOCKS5Inbound) greet(conn net.Conn) bool {
	if err := conn.SetDeadline(time.Now().Add(s.Engine.handshakeTimeout)); err != nil {
		return false
	}

	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return false
	}

	if head[0] != 0x05 {
		return false
	}

	nMethods := int(head[1])
	if nMethods <= 0 || nMethods > 255 {
		return false
	}

	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return false
	}

	for _, m := range methods {
		if m == 0x00 {
			// No-auth acceptable.
			_, _ = conn.Write([]byte{0x05, 0x00})

			return true
		}
	}

	// No acceptable method: 0xFF selection, no follow-up request.
	_, _ = conn.Write([]byte{0x05, 0xFF})

	return false
}

// readConnect reads the CONNECT request and validates it, returning
// the target address. It does NOT reply success — the engine replies
// once the outbound connection is established (failures get the
// matching error reply through pipeDialError).
func (s *SOCKS5Inbound) readConnect(conn net.Conn) (string, bool) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return "", false
	}

	if head[0] != 0x05 || head[2] != 0x00 {
		return "", false
	}

	var host string

	switch head[3] {
	case 0x01: // IPv4
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", false
		}

		host = net.IP(addr).String()
	case 0x03: // domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", false
		}

		name := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return "", false
		}

		host = string(name)
	case 0x04: // IPv6
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", false
		}

		host = net.IP(addr).String()
	default:
		s.reply(conn, 0x08) // address type not supported

		return "", false
	}

	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return "", false
	}

	port := int(binary.BigEndian.Uint16(portBytes))

	if head[1] != 0x01 {
		// BIND (0x02) and UDP ASSOCIATE (0x03) are not implemented:
		// the honest reply is command not supported (0x07).
		s.reply(conn, 0x07)

		return "", false
	}

	if port <= 0 || host == "" {
		s.reply(conn, 0x01) // general failure

		return "", false
	}

	return net.JoinHostPort(host, strconv.Itoa(port)), true
}

// reply writes a SOCKS5 reply with a zero bound address.
func (s *SOCKS5Inbound) reply(conn net.Conn, code byte) {
	// VER REP RSV ATYP(IPv4) BND.ADDR BND.PORT
	_, _ = conn.Write([]byte{0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
}

// socksSuccessReply reports a successful connect with the local side
// of the outbound connection as the bound address.
func socksSuccessReply(local net.Addr) []byte {
	host, portStr, _ := net.SplitHostPort(local.String())

	port, _ := strconv.Atoi(portStr)

	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))

	var addrType byte

	var addr []byte

	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		addrType = 0x03

		addr = append([]byte{byte(len(host))}, host...)
	case ip.To4() != nil:
		addrType = 0x01

		addr = ip.To4()
	default:
		addrType = 0x04

		addr = ip.To16()
	}

	out := []byte{0x05, 0x00, 0x00, addrType}
	out = append(out, addr...)
	out = append(out, portBytes...)

	return out
}
