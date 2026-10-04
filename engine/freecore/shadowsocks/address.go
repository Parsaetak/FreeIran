package shadowsocks

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
)

// SOCKS5 address types (RFC 1928 ATYP), as mandated for the target
// address carried in the first payload chunk.
const (
	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04
)

// maxDomainLength is the largest domain the ATYP encoding can carry
// (the length prefix is a single byte).
const maxDomainLength = 255

// AddrBytes encodes a target address in the SOCKS5 ATYP form that the
// AEAD specification uses for the target carried in the first payload
// chunk: [ATYP][address][port big-endian] with ATYP 0x01 (4-byte IPv4),
// 0x03 (length-prefixed domain) or 0x04 (16-byte IPv6). host may be an
// IP literal or a domain name (no brackets — pass the bare host).
func AddrBytes(host string, port int) ([]byte, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("shadowsocks: target port %d out of range", port)
	}
	if host == "" {
		return nil, fmt.Errorf("shadowsocks: target host must not be empty")
	}

	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			b := make([]byte, 0, 1+len(v4)+2)
			b = append(b, atypIPv4)
			b = append(b, v4...)
			return binary.BigEndian.AppendUint16(b, uint16(port)), nil
		}
		v6 := ip.To16()
		b := make([]byte, 0, 1+len(v6)+2)
		b = append(b, atypIPv6)
		b = append(b, v6...)
		return binary.BigEndian.AppendUint16(b, uint16(port)), nil
	}

	if len(host) > maxDomainLength {
		return nil, fmt.Errorf("shadowsocks: target host exceeds %d bytes", maxDomainLength)
	}
	b := make([]byte, 0, 1+1+len(host)+2)
	b = append(b, atypDomain, byte(len(host)))
	b = append(b, host...)
	return binary.BigEndian.AppendUint16(b, uint16(port)), nil
}

// readTarget parses the target address from the decrypted stream, where
// the spec places it: at the head of the first payload chunk. Reads go
// through the chunkReader, so the address may span chunk boundaries and
// any payload bytes beyond the address remain queued in the reader for
// the data phase. All reads are bounded (fixed-size or length-prefixed).
func readTarget(r *chunkReader) (string, error) {
	var atyp [1]byte
	if _, err := io.ReadFull(r, atyp[:]); err != nil {
		return "", fmt.Errorf("shadowsocks: read target ATYP: %w", err)
	}

	switch atyp[0] {
	case atypIPv4:
		var ip [4]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", fmt.Errorf("shadowsocks: read target IPv4: %w", err)
		}
		port, err := readTargetPort(r)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(net.IP(ip[:]).String(), strconv.Itoa(port)), nil

	case atypIPv6:
		var ip [16]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", fmt.Errorf("shadowsocks: read target IPv6: %w", err)
		}
		port, err := readTargetPort(r)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(net.IP(ip[:]).String(), strconv.Itoa(port)), nil

	case atypDomain:
		var length [1]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return "", fmt.Errorf("shadowsocks: read target domain length: %w", err)
		}
		if length[0] == 0 {
			return "", fmt.Errorf("shadowsocks: target domain is empty")
		}
		domain := make([]byte, length[0])
		if _, err := io.ReadFull(r, domain); err != nil {
			return "", fmt.Errorf("shadowsocks: read target domain: %w", err)
		}
		port, err := readTargetPort(r)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(string(domain), strconv.Itoa(port)), nil

	default:
		return "", fmt.Errorf("shadowsocks: unknown target ATYP 0x%02x", atyp[0])
	}
}

// readTargetPort reads the 2-byte big-endian target port. Port 0 is
// rejected: it cannot identify a TCP service and only appears in
// malformed or hostile streams.
func readTargetPort(r io.Reader) (int, error) {
	var buf [2]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, fmt.Errorf("shadowsocks: read target port: %w", err)
	}
	port := int(binary.BigEndian.Uint16(buf[:]))
	if port == 0 {
		return 0, fmt.Errorf("shadowsocks: target port is zero")
	}
	return port, nil
}
