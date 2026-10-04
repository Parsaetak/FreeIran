package netstack

import (
	"context"
	"net/netip"

	"github.com/sagernet/gvisor/pkg/buffer"
	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
)

// Test-only helpers. They live in a separate file so the production
// package has zero gVisor-facing helpers beyond the isolation wall.

func netipMustAddr4(s string) netip.Addr {
	a := netip.MustParseAddr(s)

	if !a.Is4() {
		panic("not an IPv4 address: " + s)
	}

	return a
}

func netipMustAddr6(s string) netip.Addr {
	a := netip.MustParseAddr(s)

	if !a.Is6() {
		panic("not an IPv6 address: " + s)
	}

	return a
}

func netipMustPrefix(s string) netip.Prefix {
	return netip.MustParsePrefix(s)
}

func fd00F0ab2() [16]byte {
	return netip.MustParseAddr("fd00:f0ab::2").As16()
}

// bufMake wraps raw bytes as a gVisor payload buffer.
func bufMake(data []byte) buffer.Buffer {
	return buffer.MakeWithData(data)
}

// gonetDialTCP dials a v4 destination through the client stack.
func gonetDialTCP(ctx context.Context, s *stack.Stack, addr tcpip.FullAddress) (*gonet.TCPConn, error) {
	return gonet.DialContextTCP(ctx, s, addr, header.IPv4ProtocolNumber)
}

// gonetDialTCP6 dials a v6 destination through the client stack.
func gonetDialTCP6(ctx context.Context, s *stack.Stack, addr tcpip.Address, port uint16) (*gonet.TCPConn, error) {
	return gonet.DialContextTCP(ctx, s, tcpip.FullAddress{Addr: addr, Port: port}, header.IPv6ProtocolNumber)
}
