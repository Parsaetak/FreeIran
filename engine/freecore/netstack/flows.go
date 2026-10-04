package netstack

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/sagernet/gvisor/pkg/tcpip"
)

// TCPFlow is one accepted userspace TCP connection.
type TCPFlow struct {
	// Src is the application-side source (virtual): the address the
	// client behind the TUN sent the flow from.
	Src netip.AddrPort

	// Dst is the original destination the application addressed. The
	// stack impersonates it toward the application; deciding what may
	// be done with it (relay, refuse, …) is the handler's job.
	Dst netip.AddrPort

	// Conn is the stream endpoint to the application. It is closed by
	// the handler or by Close/stack shutdown — whichever comes first;
	// closing twice is safe.
	Conn net.Conn
}

// UDPFlow is one userspace UDP socket flow.
type UDPFlow struct {
	// Src is the application-side source (virtual). It is also the
	// datagram peer of Conn: reads return datagrams the application
	// sent from Src, writes deliver datagrams back to Src.
	Src netip.AddrPort

	// Dst is the original destination the application addressed. The
	// stack impersonates it toward the application (datagrams written
	// to Conn reach the application as if they came from Dst, so the
	// handler plays the Dst-side service exactly like the TCP handler
	// sits between the application and its original destination).
	Dst netip.AddrPort

	// Conn is a connected net.PacketConn whose datagram peer is pinned
	// to Src: ReadFrom reports the real datagram source (and errors on
	// anything else), WriteTo refuses any address other than Src or
	// nil. A flow must never be repurposed into an open relay.
	Conn net.PacketConn
}

// TCPHandler receives accepted TCP flows. It MUST NOT block the packet
// loop indefinitely: the stack runs each handler in its own goroutine
// and closes flow.Conn on shutdown, which unblocks a relay-style
// handler; a handler that ignores flow.Conn can delay shutdown past
// the stack's bounded join wait.
type TCPHandler func(ctx context.Context, flow TCPFlow)

// UDPHandler receives accepted UDP flows, one per application-side
// socket 4-tuple, exactly like the TCP handler. The datagram that
// created the flow has already been queued in flow.Conn when the
// handler runs.
type UDPHandler func(ctx context.Context, flow UDPFlow)

// errForeignPeer is returned by the UDP flow conn when a datagram does
// not match the flow's pinned peer. It is a flow-contract violation,
// not a transport error: the datagram is refused, never forwarded.
var errForeignPeer = errors.New(Subsystem + ": datagram does not match the flow's pinned peer")

// statsCounters is the atomic counter set behind Stats.
type statsCounters struct {
	packetsIn, packetsOut  atomic.Int64
	droppedIn, droppedOut  atomic.Int64
	unsupported, malformed atomic.Int64
	refused                atomic.Int64
	tcpOpened, tcpClosed   atomic.Int64
	udpOpened, udpClosed   atomic.Int64
}

// flowEntry is one registered flow: a once-only closer plus a hook the
// registry sets to remove itself and count the close exactly once —
// whether the handler, or stack teardown, closes first.
type flowEntry interface {
	io.Closer

	setAfter(func())
}

// tcpFlowConn wraps the gonet TCP conn handed to a TCPHandler.
type tcpFlowConn struct {
	net.Conn

	once    sync.Once
	closeFn func() error
	after   func()
}

func newTCPFlowConn(conn net.Conn) *tcpFlowConn {
	return &tcpFlowConn{Conn: conn, closeFn: conn.Close}
}

// setAfter implements flowEntry.
func (c *tcpFlowConn) setAfter(fn func()) { c.after = fn }

// Close implements net.Conn with once-only semantics: the underlying
// endpoint is closed exactly once no matter how many parties close the
// flow (handler, stack teardown, both).
func (c *tcpFlowConn) Close() error {
	var err error

	c.once.Do(func() {
		err = c.closeFn()
		c.after()
	})

	return err
}

// udpFlowConn wraps the gonet UDP conn handed to a UDPHandler and pins
// it to the flow's application-side peer. The underlying endpoint is
// connected to that peer already; the pin exists so the flow contract
// survives any future change of the underlying conn type — reads of
// foreign datagrams and writes to foreign addresses are refused, never
// misdelivered.
type udpFlowConn struct {
	net.PacketConn

	peer    netip.AddrPort
	once    sync.Once
	closeFn func() error
	after   func()
}

func newUDPFlowConn(conn net.PacketConn, peer netip.AddrPort) *udpFlowConn {
	return &udpFlowConn{PacketConn: conn, peer: peer, closeFn: conn.Close}
}

// setAfter implements flowEntry.
func (c *udpFlowConn) setAfter(fn func()) { c.after = fn }

// Close implements net.PacketConn with once-only semantics.
func (c *udpFlowConn) Close() error {
	var err error

	c.once.Do(func() {
		err = c.closeFn()
		c.after()
	})

	return err
}

// ReadFrom implements net.PacketConn, verifying the datagram source.
func (c *udpFlowConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if err != nil {
		return n, addr, err
	}

	if from, ok := udpAddrToAddrPort(addr); !ok || from != c.peer {
		// Impossible over a connected endpoint; kept fail-closed so the
		// flow contract does not silently depend on that fact.
		return 0, nil, errForeignPeer
	}

	return n, addr, nil
}

// WriteTo implements net.PacketConn, pinning writes to the flow peer.
// A nil address means "the connected peer" (standard connected-socket
// semantics); any explicit address must be that peer.
func (c *udpFlowConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if addr == nil {
		return c.PacketConn.WriteTo(p, addrPortToUDPAddr(c.peer))
	}

	if to, ok := udpAddrToAddrPort(addr); !ok || to != c.peer {
		return 0, errForeignPeer
	}

	return c.PacketConn.WriteTo(p, addr)
}

// flowRegistry tracks open flows so Close-time teardown can close what
// the handlers did not. It is bounded indirectly: every registration
// passed the MaxFlows gate first, so the registry never holds more
// than MaxFlows entries.
type flowRegistry struct {
	mu    sync.Mutex
	flows map[flowEntry]struct{}
}

func newFlowRegistry() *flowRegistry {
	return &flowRegistry{flows: make(map[flowEntry]struct{})}
}

// register wires one flow's close-time bookkeeping (registry removal
// plus its family counter) and returns the entry handed to the
// handler.
func (r *flowRegistry) register(entry flowEntry, closedCounter *atomic.Int64) flowEntry {
	entry.setAfter(func() {
		r.remove(entry)
		closedCounter.Add(1)
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	r.flows[entry] = struct{}{}

	return entry
}

// remove drops one flow (idempotent — a flow may be removed by its own
// Close before teardown gets to it).
func (r *flowRegistry) remove(entry flowEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.flows, entry)
}

// closeAll closes every still-open flow. The per-flow Close is
// once-only, so a handler that already closed its flow is not
// double-closed here.
func (r *flowRegistry) closeAll() {
	r.mu.Lock()
	entries := make([]flowEntry, 0, len(r.flows))
	for e := range r.flows {
		entries = append(entries, e)
	}
	r.mu.Unlock()

	for _, e := range entries {
		_ = e.Close()
	}
}

// flowLimiter is the MaxFlows gate: a strict concurrent-flow cap.
// tryAcquire/release bracket every acceptance attempt, so the bound
// holds even when endpoint creation fails afterwards.
type flowLimiter struct {
	max int
	cur atomic.Int64
}

// tryAcquire claims one flow slot under the cap.
func (l *flowLimiter) tryAcquire() bool {
	for {
		n := l.cur.Load()
		if n >= int64(l.max) {
			return false
		}

		if l.cur.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// release gives one flow slot back.
func (l *flowLimiter) release() { l.cur.Add(-1) }

// addrPortOf converts a gVisor transport endpoint address/port to the
// public netip form. The ok=false case means the packet's addresses
// were not parseable — the caller refuses the flow instead of
// publishing a zero value.
func addrPortOf(addr tcpip.Address, port uint16) (netip.AddrPort, bool) {
	var ip netip.Addr

	switch addr.Len() {
	case 4:
		a4 := addr.As4()

		ip, _ = netip.AddrFromSlice(a4[:])
	case 16:
		a16 := addr.As16()

		ip, _ = netip.AddrFromSlice(a16[:])
	default:
		return netip.AddrPort{}, false
	}

	if !ip.IsValid() {
		return netip.AddrPort{}, false
	}

	return netip.AddrPortFrom(ip.Unmap(), port), true
}

// udpAddrToAddrPort converts a *net.UDPAddr (what gonet's PacketConn
// reports) into the netip form the flow contract compares.
func udpAddrToAddrPort(addr net.Addr) (netip.AddrPort, bool) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok || ua.IP == nil {
		return netip.AddrPort{}, false
	}

	ip, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return netip.AddrPort{}, false
	}

	return netip.AddrPortFrom(ip.Unmap(), uint16(ua.Port)), true
}

// addrPortToUDPAddr renders a pinned peer as the *net.UDPAddr the
// underlying gonet conn expects for connected-style writes.
func addrPortToUDPAddr(ap netip.AddrPort) *net.UDPAddr {
	ip := ap.Addr().As16()

	return &net.UDPAddr{IP: ip[:], Port: int(ap.Port())}
}
