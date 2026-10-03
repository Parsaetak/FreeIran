package tun

import (
	"context"
	"testing"
	"time"
)

// fakeDevice exercises the platform-neutral loopDevice skeleton: the
// bounded packet channel, backpressure and close semantics every
// platform implementation inherits.
type fakeDevice struct {
	loopDevice
}

func newFakeDevice(t *testing.T) (*fakeDevice, context.Context) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	d := &fakeDevice{loopDevice: newLoopDevice(DefaultIdentity(), DeviceOptions{PacketBuffer: 2})}
	t.Cleanup(func() { d.shutdown(nil) })

	return d, ctx
}

func TestLoopDeviceDeliversPackets(t *testing.T) {
	d, ctx := newFakeDevice(t)

	if !d.deliver(ctx, Packet{Data: []byte{1}}) {
		t.Fatal("deliver on an open device must succeed")
	}

	got := <-d.Packets()
	if len(got.Data) != 1 || got.Data[0] != 1 {
		t.Fatalf("packet roundtrip = %v", got)
	}
}

func TestLoopDeviceBackpressureBounded(t *testing.T) {
	// Buffer of 2: the third deliver must BLOCK (backpressure), not
	// drop and not grow the buffer.
	d, ctx := newFakeDevice(t)

	if !d.deliver(ctx, Packet{Data: []byte{1}}) || !d.deliver(ctx, Packet{Data: []byte{2}}) {
		t.Fatal("buffered delivers must succeed")
	}

	blocked := make(chan bool, 1)

	go func() { blocked <- d.deliver(ctx, Packet{Data: []byte{3}}) }()

	select {
	case ok := <-blocked:
		t.Fatalf("third deliver returned %v while the buffer was full — backpressure must block, never drop", ok)
	case <-time.After(50 * time.Millisecond):
		// Still blocked: correct.
	}

	// Draining one slot releases the blocked deliver.
	<-d.Packets()

	select {
	case ok := <-blocked:
		if !ok {
			t.Fatal("released deliver must succeed")
		}
	case <-time.After(time.Second):
		t.Fatal("draining the buffer must release the blocked deliver")
	}
}

func TestLoopDeviceCloseIsIdempotentAndWakesDeliver(t *testing.T) {
	d, _ := newFakeDevice(t)

	d.shutdown(nil)
	d.shutdown(nil) // must not panic or double-close

	if d.deliver(context.Background(), Packet{Data: []byte{1}}) {
		t.Fatal("deliver after close must report false")
	}

	// Termination is signaled through Done (the packet channel is
	// deliberately never closed — see the Device contract).
	select {
	case <-d.Done():
	default:
		t.Fatal("Done must be closed after shutdown")
	}
}

func TestLoopDeviceContextCancellation(t *testing.T) {
	d, ctx := newFakeDevice(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if d.deliver(cancelled, Packet{Data: []byte{1}}) {
		t.Fatal("deliver on a cancelled context must report false")
	}

	_ = ctx
}

func TestDeviceOptionsDefaults(t *testing.T) {
	opts := DeviceOptions{}.WithDefaults()

	if opts.MTU != DefaultMTU || opts.RingCapacity != DefaultRingCapacity ||
		opts.PacketBuffer != DefaultPacketBuffer || opts.ReadTimeout != DefaultReadTimeout {
		t.Fatalf("defaults not applied: %+v", opts)
	}
}

func TestOpenDeviceRefusesOnNonWindows(t *testing.T) {
	// On Linux this MUST refuse honestly. On Windows this test would
	// need elevation and the driver — it asserts refusal semantics
	// only where the platform has no device layer.
	d, err := OpenDevice(context.Background(), DefaultIdentity(), DeviceOptions{})

	if d != nil || err == nil {
		t.Skipf("platform has a first-party device layer (OpenDevice succeeded) — runtime adapter behavior is Phase 2 evidence")
	}

	if err != ErrUnsupportedPlatform {
		t.Fatalf("OpenDevice error = %v, want ErrUnsupportedPlatform", err)
	}
}
