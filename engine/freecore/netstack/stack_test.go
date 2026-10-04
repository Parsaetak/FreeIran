package netstack

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/freecore/tun"

	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
	"github.com/sagernet/gvisor/pkg/tcpip/link/channel"
	"github.com/sagernet/gvisor/pkg/tcpip/network/ipv4"
	"github.com/sagernet/gvisor/pkg/tcpip/network/ipv6"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
	"github.com/sagernet/gvisor/pkg/tcpip/transport/tcp"
)

// testAddrs are the two ends of the fake TUN link: .1 is the FreeIran
// interface, .2 the application host behind it.
var (
	testTUNAddr  = mustAddr("198.18.0.1")
	testAppAddr  = mustAddr("198.18.0.2")
	testFakeDst4 = mustAddr("203.0.113.7") // TEST-NET-3: never a real host
	testFakeDst6 = mustAddr("2001:db8::7") // documentation prefix
)

func mustAddr(s string) tcpip.Address {
	ip := tcpip.AddrFromSlice([]byte(net.ParseIP(s).To4()))
	if ip.Len() == 0 {
		ip = tcpip.AddrFromSlice([]byte(net.ParseIP(s).To16()))
	}

	return ip
}

// newClientStack builds the gVisor stack that plays the APPLICATION
// behind the TUN: its packets flow through the MemDevice bridge into
// the stack under test. (Tests may touch gVisor directly — the
// no-leak rule applies to the production API, not the harness.)
func newClientStack(t *testing.T, queueSize int, appAddr tcpip.Address, prefixLen int, proto tcpip.NetworkProtocolNumber) (*stack.Stack, *channel.Endpoint) {
	t.Helper()

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})

	link := channel.New(queueSize, 1280, "")

	if err := s.CreateNIC(nicID, link); err != nil {
		t.Fatalf("client CreateNIC: %s", err.String())
	}

	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		t.Fatalf("client promiscuous: %s", err.String())
	}

	if err := s.SetSpoofing(nicID, true); err != nil {
		t.Fatalf("client spoofing: %s", err.String())
	}

	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          proto,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: appAddr, PrefixLen: prefixLen},
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("client address: %s", err.String())
	}

	mask := "\x00\x00\x00\x00"
	any := tcpip.AddrFrom4([4]byte{})
	if proto == header.IPv6ProtocolNumber {
		mask = "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"
		any = tcpip.AddrFrom16([16]byte{})
	}

	sub, err := tcpip.NewSubnet(any, tcpip.MaskFrom(mask))
	if err != nil {
		t.Fatalf("subnet: %s", err.Error())
	}

	s.SetRouteTable([]tcpip.Route{{Destination: sub, NIC: nicID}})

	return s, link
}

// bridge pumps packets both ways between the client stack's link and
// the MemDevice the stack under test serves — the in-memory stand-in
// for the OS TUN wire.
func bridge(t *testing.T, device *tun.MemDevice, clientLink *channel.Endpoint, stop <-chan struct{}) {
	t.Helper()

	go func() {
		for {
			pkt := clientLink.ReadContext(context.Background())
			if pkt == nil {
				return
			}

			view := pkt.ToView()
			data := append([]byte(nil), view.AsSlice()...)
			view.Release()
			pkt.DecRef()

			select {
			case <-stop:
				return
			default:
			}

			device.InjectInbound(tun.Packet{Data: data})
		}
	}()

	go func() {
		out := device.Outbound()

		for {
			select {
			case <-stop:
				return
			case pkt := <-out:
				inject := stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: bufMake(pkt.Data),
				})

				family := header.IPv4ProtocolNumber
				if len(pkt.Data) > 0 && pkt.Data[0]>>4 == 6 {
					family = header.IPv6ProtocolNumber
				}
				clientLink.InjectInbound(family, inject)
			}
		}
	}()
}

// relayHandler is the stack-under-test's TCP handler: it connects every
// accepted flow to a REAL loopback echo server — the "first-party
// outbound" role in this test, minus the wire protocol.
func relayHandler(t *testing.T, echoAddr string) TCPHandler {
	return func(ctx context.Context, flow TCPFlow) {
		defer func() { _ = flow.Conn.Close() }()

		upstream, err := net.DialTimeout("tcp", echoAddr, 5*time.Second)
		if err != nil {
			t.Errorf("handler dial echo: %v", err)

			return
		}

		defer func() { _ = upstream.Close() }()

		done := make(chan struct{}, 2)

		go func() {
			_, _ = io.Copy(upstream, flow.Conn)

			done <- struct{}{}
		}()

		go func() {
			_, _ = io.Copy(flow.Conn, upstream)

			done <- struct{}{}
		}()

		select {
		case <-ctx.Done():
		case <-done:
		}
	}
}

// TestEndToEndTCPThroughFakeDevice is the Phase B evidence rung: real
// bytes cross the packet dataplane end to end — application stack →
// fake device → userspace IP stack → flow → real loopback echo → back.
func TestEndToEndTCPThroughFakeDevice(t *testing.T) {
	echo := newEchoServer(t)
	defer echo.Close()

	device := tun.NewMemDevice(tun.DefaultIdentity(), tun.DeviceOptions{MTU: 1280})
	defer device.Close()

	st, err := New(device, Config{
		Addr4:   netipMustAddr4("198.18.0.1"),
		Prefix4: netipMustPrefix("198.18.0.0/30"),
	}, relayHandler(t, echo.Addr().String()), nil)
	if err != nil {
		t.Fatalf("netstack.New: %v", err)
	}

	runDone := make(chan struct{})

	go func() {
		_ = st.Run(context.Background())

		close(runDone)
	}()

	stop := make(chan struct{})
	defer close(stop)

	client, clientLink := newClientStack(t, 256, tcpip.AddrFrom4([4]byte{198, 18, 0, 2}), 30, header.IPv4ProtocolNumber)
	bridge(t, device, clientLink, stop)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	full := tcpip.FullAddress{Addr: testFakeDst4, Port: 12345}

	conn, dialErr := gonetDialTCP(ctx, client, full)
	if dialErr != nil {
		t.Fatalf("application dial through TUN: %v (stats: %+v)", dialErr, st.Stats())
	}

	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	payload := make([]byte, 64*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	for i := range payload {
		if payload[i] != got[i] {
			t.Fatalf("echo mismatch at byte %d", i)
		}
	}

	_ = conn.Close()

	// Close the stack; Run must return on its own (device/ctx still
	// alive — Close is the stop signal here).
	if err := st.Close(); err != nil {
		t.Fatalf("stack Close: %v", err)
	}

	select {
	case <-runDone:
	case <-time.After(closeJoinWait + 2*time.Second):
		t.Fatal("Run did not return after Close")
	}

	stats := st.Stats()
	if stats.TCPFlowsOpened != 1 || stats.TCPFlowsClosed != 1 {
		t.Fatalf("flow counters = opened %d closed %d, want 1/1", stats.TCPFlowsOpened, stats.TCPFlowsClosed)
	}

	if stats.PacketsIn == 0 || stats.PacketsOut == 0 {
		t.Fatalf("no packets traversed the dataplane: %+v", stats)
	}

	if stats.Malformed != 0 || stats.DroppedIn != 0 || stats.DroppedOut != 0 {
		t.Fatalf("unexpected drops: %+v", stats)
	}
}

// TestEndToEndTCPOverIPv6 repeats the byte path over IPv6 (the stack
// claims v6 — this is the proof).
func TestEndToEndTCPOverIPv6(t *testing.T) {
	echo := newEchoServer(t)
	defer echo.Close()

	device := tun.NewMemDevice(tun.DefaultIdentity(), tun.DeviceOptions{MTU: 1280})
	defer device.Close()

	st, err := New(device, Config{
		Addr4:   netipMustAddr4("198.18.0.1"),
		Prefix4: netipMustPrefix("198.18.0.0/30"),
		Addr6:   netipMustAddr6("fd00:f0ab::1"),
		Prefix6: netipMustPrefix("fd00:f0ab::/64"),
	}, relayHandler(t, echo.Addr().String()), nil)
	if err != nil {
		t.Fatalf("netstack.New: %v", err)
	}

	go func() { _ = st.Run(context.Background()) }()

	stop := make(chan struct{})
	defer close(stop)

	client, clientLink := newClientStack(t, 256, tcpip.AddrFrom16(fd00F0ab2()), 64, header.IPv6ProtocolNumber)
	bridge(t, device, clientLink, stop)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, dialErr := gonetDialTCP6(ctx, client, testFakeDst6, 12345)
	if dialErr != nil {
		t.Fatalf("application dial through TUN (v6): %v", dialErr)
	}

	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	payload := []byte("freeiran tun ipv6 byte path")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: %q != %q", got, payload)
	}

	_ = conn.Close()
	_ = st.Close()
}

// TestUnsupportedUDPCounted proves the fail-closed rule: a UDP datagram
// with no UDP handler is counted as Unsupported and never surfaces as
// traffic anywhere.
func TestUnsupportedUDPCounted(t *testing.T) {
	device := tun.NewMemDevice(tun.DefaultIdentity(), tun.DeviceOptions{})
	defer device.Close()

	st, err := New(device, Config{
		Addr4:   netipMustAddr4("198.18.0.1"),
		Prefix4: netipMustPrefix("198.18.0.0/30"),
	}, func(ctx context.Context, flow TCPFlow) { _ = flow.Conn.Close() }, nil)
	if err != nil {
		t.Fatalf("netstack.New: %v", err)
	}

	go func() { _ = st.Run(context.Background()) }()

	// Minimal IPv4+UDP packet to the stack (destined to its address).
	pkt := makeUDPDatagram(netipMustAddr4("198.18.0.1").As4(), netipMustAddr4("198.18.0.2").As4())
	if !device.InjectInbound(tun.Packet{Data: pkt}) {
		t.Fatal("inject failed")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st.Stats().Unsupported == 1 {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	if got := st.Stats().Unsupported; got != 1 {
		t.Fatalf("Unsupported = %d, want 1 (UDP without handler must be classified)", got)
	}

	_ = st.Close()
}

// TestMalformedPacketsCounted: garbage and oversize never reach the
// stack's protocols.
func TestMalformedPacketsCounted(t *testing.T) {
	device := tun.NewMemDevice(tun.DefaultIdentity(), tun.DeviceOptions{})
	defer device.Close()

	st, err := New(device, Config{
		Addr4:   netipMustAddr4("198.18.0.1"),
		Prefix4: netipMustPrefix("198.18.0.0/30"),
	}, func(ctx context.Context, flow TCPFlow) { _ = flow.Conn.Close() }, nil)
	if err != nil {
		t.Fatalf("netstack.New: %v", err)
	}

	go func() { _ = st.Run(context.Background()) }()

	_ = device.InjectInbound(tun.Packet{Data: []byte{0x50, 0x01}})       // version 5
	_ = device.InjectInbound(tun.Packet{Data: []byte{0x45, 0x00, 0x00}}) // truncated v4
	_ = device.InjectInbound(tun.Packet{Data: make([]byte, 1300)})       // oversize (MTU 1280)
	_ = device.InjectInbound(tun.Packet{Data: make([]byte, 0)})          // empty

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st.Stats().Malformed == 4 {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	if got := st.Stats().Malformed; got != 4 {
		t.Fatalf("Malformed = %d, want 4", got)
	}

	_ = st.Close()
}

// TestCloseIdempotent: Close is safe under concurrency and repetition.
func TestCloseIdempotent(t *testing.T) {
	device := tun.NewMemDevice(tun.DefaultIdentity(), tun.DeviceOptions{})
	defer device.Close()

	st, err := New(device, Config{
		Addr4:   netipMustAddr4("198.18.0.1"),
		Prefix4: netipMustPrefix("198.18.0.0/30"),
	}, func(ctx context.Context, flow TCPFlow) { _ = flow.Conn.Close() }, nil)
	if err != nil {
		t.Fatalf("netstack.New: %v", err)
	}

	go func() { _ = st.Run(context.Background()) }()

	errCh := make(chan error, 3)

	for i := 0; i < 3; i++ {
		go func() {
			errCh <- st.Close()
		}()
	}

	for i := 0; i < 3; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Close hung")
		}
	}
}

// --- helpers ----------------------------------------------------------------

// newEchoServer starts the REAL loopback echo server the handler
// relays to.
func newEchoServer(t *testing.T) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer func() { _ = c.Close() }()

				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	return listener
}

// makeUDPDatagram builds one minimal IPv4+UDP packet.
func makeUDPDatagram(dst, src [4]byte) []byte {
	payload := []byte("hello udp")
	udpLen := 8 + len(payload)

	pkt := make([]byte, 20+udpLen)

	pkt[0] = 0x45 // version 4, IHL 5
	pkt[1] = 0x00 // DSCP
	pkt[2] = byte((20 + udpLen) >> 8)
	pkt[3] = byte(20 + udpLen)
	pkt[8] = 64 // TTL
	pkt[9] = 17 // UDP
	copy(pkt[12:16], src[:])
	copy(pkt[16:20], dst[:])

	udp := pkt[20:]
	udp[0], udp[1] = 0xAB, 0xCD // src port
	udp[2], udp[3] = 0x00, 0x35 // dst port 53
	udp[4], udp[5] = byte(udpLen>>8), byte(udpLen)
	copy(udp[8:], payload)

	return pkt
}
