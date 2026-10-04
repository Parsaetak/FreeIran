package tun

import (
	"bytes"
	"context"
	"errors"
)

// errMemDeviceClosed is returned by WritePacket after Close. It mirrors the
// platform devices' post-close behavior (writes to a dead adapter fail) so
// callers can treat both the same way.
var errMemDeviceClosed = errors.New("freecore.tun: mem device is closed")

// MemDevice is a platform-neutral in-memory Device: the whole Device
// contract (bounded channels, backpressure instead of silent drops,
// termination signaled through Done, idempotent Close) with the packet
// store exposed to the test instead of an OS ring buffer.
//
// Direction semantics (deliberate, do not "fix"):
//
//   - InjectInbound pushes a packet as if it had been read from the
//     wire; it becomes visible on Packets() — the path a userspace IP
//     stack reads.
//   - WritePacket sends a packet TO the wire; it becomes visible on
//     Outbound() — the path a userspace IP stack writes.
//
// WritePacket intentionally does NOT feed Packets(): a device that
// loops egress back into ingress would echo a stack's own packets into
// its receive path and corrupt every protocol state machine.
type MemDevice struct {
	loopDevice

	// outbound holds packets written via WritePacket. Like the inbound
	// packet channel it is bounded and never closed — termination is
	// signaled through Done (a closable data channel cannot be sent to
	// safely by construction, see the Device contract).
	outbound chan Packet
}

// NewMemDevice creates an in-memory Device with the provided identity.
// DeviceOptions are honored exactly like a platform device: MTU and
// PacketBuffer go through WithDefaults, so the zero options value
// yields the same negotiated sizes as the real adapter layer.
func NewMemDevice(identity Identity, opts DeviceOptions) *MemDevice {
	opts = opts.WithDefaults()

	// Both directions share the negotiated packet-buffer depth: a test
	// that forgets to drain one side then observes the same
	// backpressure a real wire would apply, not a silently growing
	// buffer.
	return &MemDevice{
		// The loopDevice skeleton is built in place (never copied):
		// it carries a sync.Once, and copying a once would copy the
		// lock the race detector rightly complains about.
		loopDevice: loopDevice{
			identity: identity,
			opts:     opts,
			packets:  make(chan Packet, opts.PacketBuffer),
			closed:   make(chan struct{}),
		},
		outbound: make(chan Packet, opts.PacketBuffer),
	}
}

// MTU implements Device.
func (d *MemDevice) MTU() int { return d.opts.MTU }

// InjectInbound pushes one packet as if read from the wire (bounded:
// it applies backpressure while the buffer is full, exactly like the
// platform read loop). It returns false when the device is closed —
// never a drop masquerading as success.
func (d *MemDevice) InjectInbound(p Packet) bool {
	// The receiver owns its copy: a test may reuse or mutate its buffer
	// right after the call without racing the device.
	return d.deliver(context.Background(), Packet{Data: bytes.Clone(p.Data)})
}

// WritePacket implements Device: it sends one packet through the
// device, which for the in-memory device means the packet lands on the
// Outbound() channel. It applies backpressure while that channel is
// full and reports an error once the device is closed.
func (d *MemDevice) WritePacket(p Packet) error {
	data := bytes.Clone(p.Data)

	select {
	case <-d.closed:
		return errMemDeviceClosed
	default:
	}

	select {
	case <-d.closed:
		// Close raced the send: refuse instead of delivering into a
		// device nobody observes anymore.
		return errMemDeviceClosed
	case d.outbound <- Packet{Data: data}:
		return nil
	}
}

// Outbound returns the bounded channel of packets written via
// WritePacket (the reverse path a test asserts against). The channel
// is never closed; receivers select over it and Done() together, as
// with Packets().
func (d *MemDevice) Outbound() <-chan Packet { return d.outbound }

// Close implements Device: it ends the session and signals Done
// exactly once. Idempotent; the packet channels are intentionally NOT
// closed (see the Device contract).
func (d *MemDevice) Close() error {
	d.shutdown(nil)

	return nil
}
