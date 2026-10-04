package freecore

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
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
)

// --- test-harness client stack (the "application" behind the TUN) ----

type clientStack struct {
	gstack *stack.Stack
	link   *channel.Endpoint
}

func newClientStack(t *testing.T, appAddr netip.Addr, prefixLen int) *clientStack {
	t.Helper()

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})

	link := channel.New(256, uint32(tun.DefaultMTU), "")

	if err := s.CreateNIC(1, link); err != nil {
		t.Fatalf("client CreateNIC: %s", err)
	}

	if err := s.SetPromiscuousMode(1, true); err != nil {
		t.Fatalf("client promiscuous: %s", err)
	}

	if err := s.SetSpoofing(1, true); err != nil {
		t.Fatalf("client spoofing: %s", err)
	}

	proto := header.IPv4ProtocolNumber
	if appAddr.Is6() {
		proto = header.IPv6ProtocolNumber
	}

	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          proto,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addrToTcpipTest(appAddr), PrefixLen: prefixLen},
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("client address: %s", err)
	}

	mask := [4]byte{}
	any := tcpip.AddrFrom4(mask)
	if appAddr.Is6() {
		any = tcpip.AddrFrom16([16]byte{})
	}

	sub, err := tcpip.NewSubnet(any, tcpip.MaskFrom(zerosFor(appAddr)))
	if err != nil {
		t.Fatalf("client subnet: %s", err)
	}

	s.SetRouteTable([]tcpip.Route{{Destination: sub, NIC: 1}})

	return &clientStack{gstack: s, link: link}
}

func zerosFor(addr netip.Addr) string {
	if addr.Is6() {
		return "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"
	}

	return "\x00\x00\x00\x00"
}

func addrToTcpipTest(addr netip.Addr) tcpip.Address {
	if addr.Is4() {
		a4 := addr.As4()

		return tcpip.AddrFrom4(a4)
	}

	a16 := addr.As16()

	return tcpip.AddrFrom16(a16)
}

// bridge pumps packets between the client stack and the MemDevice the
// dataplane serves — the in-memory stand-in for the OS TUN wire.
func bridgeStacks(t *testing.T, device *tun.MemDevice, client *clientStack, stop <-chan struct{}) {
	t.Helper()

	go func() {
		for {
			pkt := client.link.ReadContext(context.Background())
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
		for {
			select {
			case <-stop:
				return
			case pkt := <-device.Outbound():
				family := header.IPv4ProtocolNumber
				if len(pkt.Data) > 0 && pkt.Data[0]>>4 == 6 {
					family = header.IPv6ProtocolNumber
				}

				inject := stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: buffer.MakeWithData(pkt.Data),
				})

				client.link.InjectInbound(family, inject)
			}
		}
	}()
}

// --- the marquee test --------------------------------------------------------

// TestTUNDataplaneEndToEndThroughSOCKS5Remote is the release's central
// evidence rung: application packets → fake TUN device → FreeIran
// userspace IP stack → engine session → Router (default policy →
// PROXY) → first-party SOCKS5 outbound → loopback SOCKS5 fixture →
// real echo server → back through every layer. Real bytes, both
// directions, one authority.
func TestTUNDataplaneEndToEndThroughSOCKS5Remote(t *testing.T) {

	remoteAddr := localSOCKS5Remote(t)

	remoteHost, remotePortStr, _ := net.SplitHostPort(remoteAddr)

	var remotePort int

	_, _ = fmtSscan(remotePortStr, &remotePort)

	device := tun.NewMemDevice(tun.DefaultIdentity(), tun.DeviceOptions{})

	route := Route{
		ConfigID:    "tun-dataplane-it",
		Outbound:    OutboundSOCKS5,
		Endpoint:    Endpoint{Host: remoteHost, Port: remotePort},
		Network:     NetworkTCP,
		Security:    SecurityNone,
		DialTimeout: 3 * time.Second,
	}

	engine, err := NewEngine(Options{
		Route:     route,
		LocalHost: "127.0.0.1",
		LocalPort: freePort(t),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if err := engine.Run(context.Background()); err != nil {
		t.Fatalf("engine Run: %v", err)
	}

	dp, err := NewTUNDataplane(engine, TUNDataplaneOptions{Device: device})
	if err != nil {
		t.Fatalf("NewTUNDataplane: %v", err)
	}

	runDone := make(chan struct{})

	go func() {
		_ = dp.Run(context.Background())

		close(runDone)
	}()

	client := newClientStack(t, netip.MustParseAddr("198.18.0.2"), 30)

	stop := make(chan struct{})

	bridgeStacks(t, device, client, stop)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, dialErr := gonet.DialContextTCP(ctx, client.gstack, tcpip.FullAddress{
		Addr: tcpip.AddrFrom4([4]byte{203, 0, 113, 7}),
		Port: 12345,
	}, header.IPv4ProtocolNumber)
	if dialErr != nil {
		t.Fatalf("application dial through the dataplane: %v (packets: %+v)", dialErr, dp.PacketStats())
	}

	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	payload := make([]byte, 32*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write through dataplane: %v", err)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo through dataplane: %v", err)
	}

	for i := range payload {
		if payload[i] != got[i] {
			t.Fatalf("byte path corrupted at %d", i)
		}
	}

	_ = conn.Close()

	// The flow was an ordinary engine session.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && engine.registry.Len() > 0 {
		time.Sleep(10 * time.Millisecond)
	}

	stats := dp.PacketStats()
	if stats.TCPFlowsOpened != 1 {
		t.Fatalf("TCP flows opened = %d, want 1", stats.TCPFlowsOpened)
	}

	if stats.TCPFlowsClosed != 1 {
		t.Fatalf("TCP flows closed = %d, want 1", stats.TCPFlowsClosed)
	}

	if stats.Unsupported != 0 || stats.Malformed != 0 {
		t.Fatalf("unexpected classifications: %+v", stats)
	}

	close(stop)

	if err := dp.Close(); err != nil {
		t.Fatalf("dataplane close: %v", err)
	}

	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("dataplane Run did not return after Close")
	}

	engine.Stop(time.Second)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()

	if err := engine.Wait(waitCtx); err != nil {
		t.Fatalf("engine Wait after dataplane stop = %v, want nil", err)
	}
}

// TestTUNDataplaneIPv6ThroughSOCKS5Remote repeats the byte path over
// IPv6 (the stack and the engine are family-agnostic).
func TestTUNDataplaneIPv6ThroughSOCKS5Remote(t *testing.T) {

	remoteAddr := localSOCKS5Remote(t)

	remoteHost, remotePortStr, _ := net.SplitHostPort(remoteAddr)

	var remotePort int

	_, _ = fmtSscan(remotePortStr, &remotePort)

	device := tun.NewMemDevice(tun.DefaultIdentity(), tun.DeviceOptions{})

	route := Route{
		Outbound:    OutboundSOCKS5,
		Endpoint:    Endpoint{Host: remoteHost, Port: remotePort},
		Network:     NetworkTCP,
		Security:    SecurityNone,
		DialTimeout: 3 * time.Second,
	}

	engine, err := NewEngine(Options{
		Route:     route,
		LocalHost: "127.0.0.1",
		LocalPort: freePort(t),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if err := engine.Run(context.Background()); err != nil {
		t.Fatalf("engine Run: %v", err)
	}

	dp, err := NewTUNDataplane(engine, TUNDataplaneOptions{
		Device:  device,
		Addr4:   netip.MustParseAddr(DefaultTUNAddr4),
		Prefix4: netip.MustParsePrefix(DefaultTUNPrefix4),
		Addr6:   netip.MustParseAddr(DefaultTUNAddr6),
		Prefix6: netip.MustParsePrefix(DefaultTUNPrefix6),
	})
	if err != nil {
		t.Fatalf("NewTUNDataplane: %v", err)
	}

	go func() { _ = dp.Run(context.Background()) }()

	client := newClientStack(t, netip.MustParseAddr("fd00:f0ab::2"), 64)

	stop := make(chan struct{})

	bridgeStacks(t, device, client, stop)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, dialErr := gonet.DialContextTCP(ctx, client.gstack, tcpip.FullAddress{
		Addr: tcpip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7}),
		Port: 12345,
	}, header.IPv6ProtocolNumber)
	if dialErr != nil {
		t.Fatalf("v6 application dial: %v (packets: %+v)", dialErr, dp.PacketStats())
	}

	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	payload := []byte("freeiran first-party tun ipv6 dataplane through socks5")

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("v6 write: %v", err)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("v6 read echo: %v", err)
	}

	if string(got) != string(payload) {
		t.Fatalf("v6 byte path corrupted: %q != %q", got, payload)
	}

	_ = conn.Close()
	close(stop)
	_ = dp.Close()
	engine.Stop(time.Second)
}

// --- Phase D: loop prevention dialer (deterministic parts) -------------------

// TestUpstreamDialerRefusesWithoutHonorableConstraint proves the
// fail-closed rule: when the platform cannot honor the exclusion
// (non-Windows in this environment), the upstream dial is REFUSED —
// never silently unbound, because an unbound upstream is a loop risk.
func TestUpstreamDialerRefusesWithoutHonorableConstraint(t *testing.T) {
	constraint := &tun.UpstreamConstraint{ExcludedInterfaceIndex: 9}

	dialer := NewUpstreamDialer(constraint)

	_, err := dialer.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil {
		t.Fatal("upstream dial succeeded without an honorable constraint — loop risk")
	}
}

// TestUpstreamDialerPlainWithoutTUN: no constraint → plain system
// dialing (System Proxy mode dials the remote directly, as always).
func TestUpstreamDialerPlainWithoutTUN(t *testing.T) {
	dialer := NewUpstreamDialer(nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer func() { _ = ln.Close() }()

	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
	}()

	conn, err := dialer.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("plain upstream dial: %v", err)
	}

	_ = conn.Close()
}

// TestUpstreamDialerObservesLiveInterface pins the "no hardcoded
// adapter" rule: the binding must be derived from CURRENT OS
// observation. The hook contract is exercised with a stub observation
// window (the Windows implementation is compile-verified; the
// decision logic here is platform-neutral).
func TestUpstreamDialerObservesLiveInterface(t *testing.T) {
	var bound atomic.Int32

	original := upstreamBinding
	upstreamBinding = func(index int) func(network, address string) (net.Conn, error) {
		return func(network, address string) (net.Conn, error) {
			bound.Store(int32(index))

			return net.Dial(network, address)
		}
	}

	t.Cleanup(func() { upstreamBinding = original })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer func() { _ = ln.Close() }()

	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
	}()

	dialer := NewUpstreamDialer(&tun.UpstreamConstraint{ExcludedInterfaceIndex: 42})

	conn, err := dialer.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("bound dial: %v", err)
	}

	_ = conn.Close()

	if got := bound.Load(); got != 42 {
		t.Fatalf("binding observed index %d, want 42 (the constraint's exclusion target)", got)
	}
}
