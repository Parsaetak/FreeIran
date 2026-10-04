package tun

import (
	"bytes"
	"testing"
	"time"
)

func newMemDevice(t *testing.T, opts DeviceOptions) *MemDevice {
	t.Helper()

	d := NewMemDevice(DefaultIdentity(), opts)
	t.Cleanup(func() { _ = d.Close() })

	return d
}

func TestMemDeviceRoundTrip(t *testing.T) {
	d := newMemDevice(t, DeviceOptions{MTU: 1500, PacketBuffer: 4})

	if d.MTU() != 1500 {
		t.Fatalf("MTU() = %d, want negotiated option 1500", d.MTU())
	}

	// Inbound: what InjectInbound pushes is what the stack reads on
	// Packets().
	in := []byte{0x45, 1, 2, 3}
	if !d.InjectInbound(Packet{Data: in}) {
		t.Fatal("InjectInbound on an open device must succeed")
	}

	// The device owns its copy: mutating the caller's buffer after the
	// call must not race or corrupt the queued packet.
	in[0] = 0xff

	select {
	case got := <-d.Packets():
		if !bytes.Equal(got.Data, []byte{0x45, 1, 2, 3}) {
			t.Fatalf("inbound packet = %v, want the pushed bytes", got.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("injected packet never appeared on Packets()")
	}

	// Outbound: what WritePacket sends is what the wire side reads on
	// Outbound().
	out := []byte{0x60, 4, 5, 6}
	if err := d.WritePacket(Packet{Data: out}); err != nil {
		t.Fatalf("WritePacket on an open device: %v", err)
	}

	out[1] = 0xee

	select {
	case got := <-d.Outbound():
		if !bytes.Equal(got.Data, []byte{0x60, 4, 5, 6}) {
			t.Fatalf("outbound packet = %v, want the written bytes", got.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("written packet never appeared on Outbound()")
	}
}

func TestMemDeviceBoundedBothDirections(t *testing.T) {
	// Capacity 2 in each direction: the third push must BLOCK
	// (backpressure), not drop and not grow the buffer — on both the
	// inbound and the outbound path.
	d := newMemDevice(t, DeviceOptions{PacketBuffer: 2})

	if !d.InjectInbound(Packet{Data: []byte{1}}) || !d.InjectInbound(Packet{Data: []byte{2}}) {
		t.Fatal("buffered inbound pushes must succeed")
	}

	inBlocked := make(chan bool, 1)

	go func() { inBlocked <- d.InjectInbound(Packet{Data: []byte{3}}) }()

	select {
	case ok := <-inBlocked:
		t.Fatalf("third InjectInbound returned %v while full — must block, never drop", ok)
	case <-time.After(50 * time.Millisecond):
		// Still blocked: correct.
	}

	<-d.Packets()

	select {
	case ok := <-inBlocked:
		if !ok {
			t.Fatal("released inbound push must succeed")
		}
	case <-time.After(time.Second):
		t.Fatal("draining Packets() must release the blocked InjectInbound")
	}

	if err := d.WritePacket(Packet{Data: []byte{1}}); err != nil {
		t.Fatalf("buffered outbound write: %v", err)
	}

	if err := d.WritePacket(Packet{Data: []byte{2}}); err != nil {
		t.Fatalf("buffered outbound write: %v", err)
	}

	outBlocked := make(chan error, 1)

	go func() { outBlocked <- d.WritePacket(Packet{Data: []byte{3}}) }()

	select {
	case err := <-outBlocked:
		t.Fatalf("third WritePacket returned %v while full — must block, never drop", err)
	case <-time.After(50 * time.Millisecond):
		// Still blocked: correct.
	}

	<-d.Outbound()

	select {
	case err := <-outBlocked:
		if err != nil {
			t.Fatalf("released outbound write: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("draining Outbound() must release the blocked WritePacket")
	}
}

func TestMemDeviceCloseSemantics(t *testing.T) {
	d := newMemDevice(t, DeviceOptions{})

	if err := d.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Idempotent: a second Close must not panic or double-signal.
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	select {
	case <-d.Done():
	default:
		t.Fatal("Done must be closed after Close")
	}

	// Refuse further work honestly — false / error, never a silent drop
	// or a panic.
	if d.InjectInbound(Packet{Data: []byte{1}}) {
		t.Fatal("InjectInbound after Close must report false")
	}

	if err := d.WritePacket(Packet{Data: []byte{1}}); err == nil {
		t.Fatal("WritePacket after Close must report an error")
	}
}

func TestMemDeviceDefaults(t *testing.T) {
	d := NewMemDevice(DefaultIdentity(), DeviceOptions{})
	t.Cleanup(func() { _ = d.Close() })

	if d.MTU() != DefaultMTU {
		t.Fatalf("default MTU = %d, want %d", d.MTU(), DefaultMTU)
	}

	// PacketBuffer default: the channels are bounded at DefaultPacketBuffer
	// slots. Fill the inbound side to the brim without blocking to prove
	// the capacity.
	for i := range DefaultPacketBuffer {
		if !d.InjectInbound(Packet{Data: []byte{byte(i)}}) {
			t.Fatalf("push %d into a fresh device must succeed", i)
		}
	}

	blocked := make(chan bool, 1)

	go func() { blocked <- d.InjectInbound(Packet{Data: []byte{0}}) }()

	select {
	case ok := <-blocked:
		t.Fatalf("push beyond default capacity returned %v — buffer grew or dropped", ok)
	case <-time.After(50 * time.Millisecond):
		// Still blocked: capacity is the default bound.
	}
}
