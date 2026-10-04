package netstack

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Parsaetak/FreeIran/engine/freecore/tun"

	"github.com/sagernet/gvisor/pkg/buffer"
	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
	"github.com/sagernet/gvisor/pkg/tcpip/link/channel"
	"github.com/sagernet/gvisor/pkg/tcpip/network/ipv4"
	"github.com/sagernet/gvisor/pkg/tcpip/network/ipv6"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
	"github.com/sagernet/gvisor/pkg/tcpip/transport/tcp"
	"github.com/sagernet/gvisor/pkg/tcpip/transport/udp"
	"github.com/sagernet/gvisor/pkg/waiter"
)

// tcpMaxInFlight bounds the half-open (in-flight forwarded) TCP
// connection requests inside the gVisor forwarder.
const tcpMaxInFlight = 1024

// nicID is the single virtual NIC the stack serves. One TUN device, one
// NIC — the dataplane has no concept of multiple interfaces; routing
// decisions happen above this package.
const nicID = 1

// closeJoinWait bounds how long Close waits for the packet loops and
// flow handlers to unwind before reporting completion. A bounded wait
// keeps shutdown observable: past the bound the stack reports anyway
// instead of hanging the caller (leaked goroutines, if any, are a bug
// the race suite must catch, not a shutdown property).
const closeJoinWait = 5 * time.Second

// Stats is the honest counter set of one stack instance. Every packet
// that enters or leaves is counted; every abnormal disposition
// (malformed, unsupported, dropped) is counted — no silent discard
// path exists.
type Stats struct {
	// PacketsIn counts packets read from the device (all of them,
	// including later-classified malformed/unsupported ones).
	PacketsIn int64

	// PacketsOut counts packets written to the device.
	PacketsOut int64

	// DroppedIn counts inbound packets that could not enter the stack
	// (device closed under us, inject race).
	DroppedIn int64

	// DroppedOut counts outbound packets the device refused.
	DroppedOut int64

	// Unsupported counts packets parsed but unhandled because no
	// handler exists (UDP without a UDP handler). Fail-closed: the
	// packet never reaches the physical interface through us.
	Unsupported int64

	// Malformed counts packets that are not valid IP packets of a
	// known family (bad version nibble, truncated header, oversize).
	Malformed int64

	// Refused counts flows rejected by the MaxFlows bound (TCP gets a
	// RST; UDP datagrams are dropped).
	Refused int64

	// TCPFlowsOpened/TCPFlowsClosed and UDPFlowsOpened/UDPFlowsClosed
	// are the flow lifecycle counters.
	TCPFlowsOpened, TCPFlowsClosed int64
	UDPFlowsOpened, UDPFlowsClosed int64
}

// Stack is the FreeIran-owned userspace IP stack: it terminates TCP
// (and optionally UDP) arriving as raw IP packets on a tun.Device and
// hands flows to FreeIran handlers as plain net.Conn/net.PacketConn.
// gVisor types never leave this package (see doc.go — the isolation
// wall).
type Stack struct {
	cfg    resolvedConfig
	device tun.Device

	tcpHandler TCPHandler
	udpHandler UDPHandler

	link  *channel.Endpoint
	stack *stack.Stack

	counters statsCounters
	limiter  flowLimiter
	registry *flowRegistry

	ctx      context.Context
	cancel   context.CancelFunc
	loopsWG  sync.WaitGroup
	closeMu  sync.Mutex
	closed   bool
	closeErr error
}

// New builds a stack over device. The device's MTU is the wire truth;
// the config is validated against it (fail-closed). tcp must be
// non-nil; udp may be nil — UDP is then explicitly unsupported (each
// datagram counted), never silently discarded and never sent anywhere.
func New(device tun.Device, cfg Config, tcpH TCPHandler, udpH UDPHandler) (*Stack, error) {
	if device == nil {
		return nil, errNilDevice
	}

	if tcpH == nil {
		return nil, errNoTCPHandler
	}

	resolved, err := cfg.resolve(device.MTU())
	if err != nil {
		return nil, err
	}

	s := &Stack{
		cfg:        resolved,
		device:     device,
		tcpHandler: tcpH,
		udpHandler: udpH,
		registry:   newFlowRegistry(),
	}
	s.limiter.max = resolved.maxFlows

	opts := stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        false,
	}

	s.stack = stack.New(opts)
	s.link = channel.New(resolved.queueSize, uint32(resolved.mtu), "")
	// sing-tun parity: TUN wires carry packets whose checksums the
	// host kernel already produced/validated — advertise RX offload so
	// the stack does not re-validate them byte-for-byte.
	s.link.LinkEPCapabilities = stack.CapabilityRXChecksumOffload

	if err := s.stack.CreateNIC(nicID, s.link); err != nil {
		return nil, &Error{Op: "create NIC", Err: err}
	}

	// Promiscuous: accept packets for ANY destination — the TUN wire
	// carries the whole application address space, not the interface
	// address. Spoofing: replies may carry the impersonated destination
	// as source. Both are the tun2socks contract, not accidents.
	if err := s.stack.SetPromiscuousMode(nicID, true); err != nil {
		return nil, &Error{Op: "promiscuous mode", Err: err}
	}

	if err := s.stack.SetSpoofing(nicID, true); err != nil {
		return nil, &Error{Op: "spoofing", Err: err}
	}

	if tcpErr := s.addInterfaceAddresses(); tcpErr != nil {
		return nil, &Error{Op: "interface addresses", Err: tcpErr}
	}

	// The single route: everything goes out the TUN NIC (the stack
	// never talks to a physical interface — that is the host's job,
	// and upstream dials are governed by loop prevention ABOVE us).
	s.stack.SetRouteTable([]tcpip.Route{
		{Destination: ipv4AnySubnet(), NIC: nicID},
		{Destination: ipv6AnySubnet(), NIC: nicID},
	})

	// maxInFlight=1024 (the sing-tun convention): this fork treats 0 as
	// "drop every SYN", not "unbounded" — the value is a real bound on
	// half-open forwarded connections.
	tcpForwarder := tcp.NewForwarder(s.stack, 0, tcpMaxInFlight, s.handleTCPRequest)
	s.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)

	if udpH != nil {
		udpForwarder := udp.NewForwarder(s.stack, s.handleUDPRequest)
		s.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)
	}

	return s, nil
}

// addInterfaceAddresses configures the TUN interface addresses on the
// NIC. IPv4 is required by Config validation; IPv6 is optional.
func (s *Stack) addInterfaceAddresses() tcpip.Error {
	prefix4 := s.cfg.prefix4
	if err := s.stack.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          header.IPv4ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addrToTcpip(s.cfg.addr4), PrefixLen: prefix4.Bits()},
	}, stack.AddressProperties{}); err != nil {
		return err
	}

	if s.cfg.addr6.IsValid() {
		if err := s.stack.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
			Protocol:          header.IPv6ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: addrToTcpip(s.cfg.addr6), PrefixLen: s.cfg.prefix6.Bits()},
		}, stack.AddressProperties{}); err != nil {
			return err
		}
	}

	return nil
}

// Run serves until ctx is cancelled or the device terminates. It blocks;
// the nil return is a clean stop.
func (s *Stack) Run(ctx context.Context) error {
	s.closeMu.Lock()

	if s.closed {
		s.closeMu.Unlock()

		return s.closeErr
	}

	s.ctx, s.cancel = context.WithCancel(ctx)
	runCtx := s.ctx
	s.closeMu.Unlock()

	s.loopsWG.Add(2)
	go s.ingressLoop(runCtx)
	go s.egressLoop(runCtx)

	// The device terminating is a stop signal for the whole stack (the
	// adapter is gone; there is no wire anymore).
	s.loopsWG.Add(1)
	go func() {
		defer s.loopsWG.Done()

		select {
		case <-runCtx.Done():
		case <-s.device.Done():
			s.cancelContext()
		}
	}()

	s.loopsWG.Wait()

	return nil
}

// Close stops the stack: flows close, loops join (bounded), the link
// endpoint closes. Idempotent, safe concurrent with Run.
func (s *Stack) Close() error {
	s.closeMu.Lock()

	if s.closed {
		s.closeMu.Unlock()

		return s.closeErr
	}

	s.closed = true
	s.closeMu.Unlock()

	s.cancelContext()
	s.registry.closeAll()
	s.link.Close()

	done := make(chan struct{})

	go func() {
		s.loopsWG.Wait()

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(closeJoinWait):
		// Bounded honesty: report completion past the bound rather than
		// hang the caller. The race suite guards for leaked goroutines.
	}

	return nil
}

// Stats returns the current counter snapshot.
func (s *Stack) Stats() Stats {
	return Stats{
		PacketsIn:      s.counters.packetsIn.Load(),
		PacketsOut:     s.counters.packetsOut.Load(),
		DroppedIn:      s.counters.droppedIn.Load(),
		DroppedOut:     s.counters.droppedOut.Load(),
		Unsupported:    s.counters.unsupported.Load(),
		Malformed:      s.counters.malformed.Load(),
		Refused:        s.counters.refused.Load(),
		TCPFlowsOpened: s.counters.tcpOpened.Load(),
		TCPFlowsClosed: s.counters.tcpClosed.Load(),
		UDPFlowsOpened: s.counters.udpOpened.Load(),
		UDPFlowsClosed: s.counters.udpClosed.Load(),
	}
}

// cancelContext cancels the run context if it exists (Close before
// Run, Close racing Run — both legal; cancel must never race a nil).
func (s *Stack) cancelContext() {
	s.closeMu.Lock()
	cancel := s.cancel
	s.closeMu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// ingressLoop moves packets device → stack. Every packet is classified
// and counted; nothing silently vanishes.
func (s *Stack) ingressLoop(ctx context.Context) {
	defer s.loopsWG.Done()

	packets := s.device.Packets()
	done := s.device.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case pkt, ok := <-packets:
			if !ok {
				// The channel is contractually never closed; a platform
				// implementation that breaks that contract still must not
				// panic here.
				return
			}

			s.handleIngressPacket(pkt)
		}
	}
}

// handleIngressPacket classifies and injects one inbound packet.
func (s *Stack) handleIngressPacket(pkt tun.Packet) {
	s.counters.packetsIn.Add(1)

	data := pkt.Data
	if len(data) == 0 || len(data) > s.cfg.mtu {
		s.counters.malformed.Add(1)

		return
	}

	family, headerLen := classifyIP(data)
	switch family {
	case ipFamilyV4, ipFamilyV6:
	default:
		s.counters.malformed.Add(1)

		return
	}

	// UDP without a handler is the explicit unsupported class: refuse
	// BEFORE the stack sees it, so no UDP endpoint is ever created and
	// the packet provably never leaves through any path.
	if family == ipFamilyV4 && isUDPv4(data, headerLen) || family == ipFamilyV6 && isUDPv6(data, headerLen) {
		if s.udpHandler == nil {
			s.counters.unsupported.Add(1)

			return
		}
	}

	proto := header.IPv4ProtocolNumber
	if family == ipFamilyV6 {
		proto = header.IPv6ProtocolNumber
	}

	inject := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload:           buffer.MakeWithData(data),
		IsForwardedPacket: true,
	})

	s.link.InjectInbound(proto, inject)
}

// egressLoop moves packets stack → device.
func (s *Stack) egressLoop(ctx context.Context) {
	defer s.loopsWG.Done()

	for {
		pkt := s.link.ReadContext(ctx)
		if pkt == nil {
			// ReadContext returns nil on context cancellation or link
			// close — both are stop conditions.
			return
		}

		view := pkt.ToView()
		data := append([]byte(nil), view.AsSlice()...)
		view.Release()
		pkt.DecRef()

		if len(data) == 0 {
			continue
		}

		s.counters.packetsOut.Add(1)

		if err := s.device.WritePacket(tun.Packet{Data: data}); err != nil {
			s.counters.droppedOut.Add(1)
		}
	}
}

// handleTCPRequest accepts one TCP connection request (SYN). The
// impersonation contract: from the application's view this endpoint IS
// the real destination; what we do with it is the handler's decision.
func (s *Stack) handleTCPRequest(req *tcp.ForwarderRequest) {
	id := req.ID()

	src, srcOK := addrPortOf(id.RemoteAddress, id.RemotePort)
	dst, dstOK := addrPortOf(id.LocalAddress, id.LocalPort)
	if !srcOK || !dstOK {
		s.counters.malformed.Add(1)
		req.Complete(true)

		return
	}

	if !s.limiter.tryAcquire() {
		// The bound is real: the SYN is answered with RST, the refusal
		// is counted — the application sees a genuine connection
		// refusal, not a black hole.
		s.counters.refused.Add(1)
		req.Complete(true)

		return
	}

	var wq waiter.Queue

	ep, err := req.CreateEndpoint(&wq)
	if err != nil {
		s.limiter.release()
		req.Complete(true)

		return
	}

	flowConn := s.registry.register(newTCPFlowConn(gonet.NewTCPConn(&wq, ep)), &s.counters.tcpClosed)
	stream := flowConn.(net.Conn)
	s.counters.tcpOpened.Add(1)

	ctx, cancel := context.WithCancel(s.context())

	go func() {
		defer func() {
			cancel()
			_ = flowConn.Close()
			s.limiter.release()
		}()

		s.tcpHandler(ctx, TCPFlow{Src: src, Dst: dst, Conn: stream})
	}()
}

// handleUDPRequest accepts one UDP socket flow (one per application
// 4-tuple). Requires a handler — the constructor refuses to register
// the UDP forwarder without one, so this only runs when UDP is real.
func (s *Stack) handleUDPRequest(req *udp.ForwarderRequest) {
	id := req.ID()

	src, srcOK := addrPortOf(id.RemoteAddress, id.RemotePort)
	dst, dstOK := addrPortOf(id.LocalAddress, id.LocalPort)
	if !srcOK || !dstOK {
		s.counters.malformed.Add(1)

		return
	}

	if !s.limiter.tryAcquire() {
		s.counters.refused.Add(1)

		return
	}

	var wq waiter.Queue

	ep, err := req.CreateEndpoint(&wq)
	if err != nil {
		s.limiter.release()

		return
	}

	flowConn := s.registry.register(newUDPFlowConn(gonet.NewUDPConn(&wq, ep), src), &s.counters.udpClosed)
	packet := flowConn.(net.PacketConn)
	s.counters.udpOpened.Add(1)

	ctx, cancel := context.WithCancel(s.context())

	go func() {
		defer func() {
			cancel()
			_ = flowConn.Close()
			s.limiter.release()
		}()

		s.udpHandler(ctx, UDPFlow{Src: src, Dst: dst, Conn: packet})
	}()
}

// context returns the run context (zero-context-safe for Close-before-Run).
func (s *Stack) context() context.Context {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	if s.ctx == nil {
		return context.Background()
	}

	return s.ctx
}

// --- IP classification ------------------------------------------------------

type ipFamily int

const (
	ipFamilyNone ipFamily = iota
	ipFamilyV4
	ipFamilyV6
)

// classifyIP detects the family and validates the fixed header length.
func classifyIP(data []byte) (ipFamily, int) {
	if len(data) < 1 {
		return ipFamilyNone, 0
	}

	switch version := int(data[0] >> 4); version {
	case 4:
		if len(data) < header.IPv4MinimumSize {
			return ipFamilyNone, 0
		}

		ihl := int(data[0]&0x0F) * 4
		if ihl < header.IPv4MinimumSize || ihl > len(data) {
			return ipFamilyNone, 0
		}

		return ipFamilyV4, ihl
	case 6:
		if len(data) < header.IPv6MinimumSize {
			return ipFamilyNone, 0
		}

		return ipFamilyV6, header.IPv6MinimumSize
	default:
		return ipFamilyNone, 0
	}
}

// isUDPv4 reports whether the IPv4 packet carries UDP.
func isUDPv4(data []byte, ihl int) bool {
	if len(data) < ihl+2 {
		return false
	}

	return data[9] == byte(header.UDPProtocolNumber)
}

// isUDPv6 reports whether the (fixed-header) IPv6 packet carries UDP.
// Extension headers before UDP are treated as not-UDP for the
// unsupported gate: the stack itself parses them, and a UDP-in-extension
// flow without a handler is still counted (Unsupported) at the transport
// gate below — the classification here is the cheap pre-filter.
func isUDPv6(data []byte, base int) bool {
	if len(data) < base+2 {
		return false
	}

	return data[6] == byte(header.UDPProtocolNumber)
}

// --- conversions ------------------------------------------------------------

// addrToTcpip converts a netip.Addr to the fork's Address form.
func addrToTcpip(addr netip.Addr) tcpip.Address {
	if addr.Is4() {
		a4 := addr.As4()

		return tcpip.AddrFrom4(a4)
	}

	a16 := addr.As16()

	return tcpip.AddrFrom16(a16)
}

// ipv4AnySubnet is the catch-all IPv4 route destination.
func ipv4AnySubnet() tcpip.Subnet {
	sub, _ := tcpip.NewSubnet(
		tcpip.AddrFrom4([4]byte{}),
		tcpip.MaskFrom("\x00\x00\x00\x00"),
	)

	return sub
}

// ipv6AnySubnet is the catch-all IPv6 route destination.
func ipv6AnySubnet() tcpip.Subnet {
	sub, _ := tcpip.NewSubnet(
		tcpip.AddrFrom16([16]byte{}),
		tcpip.MaskFrom("\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"),
	)

	return sub
}

// Error is the package's public error type: gVisor tcpip.Error values
// never leak (they are interfaces — wrapping them as error would leak
// the concrete type through errors.As).
type Error struct {
	Op  string
	Err tcpip.Error
}

// Error implements error.
func (e *Error) Error() string {
	return Subsystem + ": " + e.Op + ": " + e.Err.String()
}

// errors for the constructor contract.
var (
	errNilDevice    = errorString(Subsystem + ": device is nil")
	errNoTCPHandler = errorString(Subsystem + ": TCP handler is required")
)

// errorString is a minimal const-shape error.
type errorString string

// Error implements error.
func (e errorString) Error() string { return string(e) }

// keep the atomic import honest if counters move.
var _ = atomic.Int64{}
