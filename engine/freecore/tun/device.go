package tun

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Device is the first-party TUN device boundary: the Phase 2 packet
// dataplane reads and writes packets through exactly this interface,
// over the bounded packet channel — no raw ring exposure, no
// unbounded buffering.
type Device interface {
	// Identity returns the deterministic FreeIran adapter identity
	// this device was opened with.
	Identity() Identity

	// MTU returns the negotiated packet size.
	MTU() int

	// Packets returns the bounded inbound packet channel. The
	// implementation owns the read loop; when the buffer is full the
	// loop applies backpressure (never drops silently). The channel
	// is NEVER closed: termination is signaled through Done() — a
	// closable data channel cannot be sent to safely by
	// construction (select-send on a channel that Close may close is
	// a panic, not a race detector artifact).
	Packets() <-chan Packet

	// Done is closed exactly once when the device terminates. Receivers
	// select over Packets() and Done() together.
	Done() <-chan struct{}

	// WritePacket sends one packet through the device.
	WritePacket(p Packet) error

	// Close ends the session and removes the adapter. Idempotent.
	Close() error
}

// Packet is one TUN frame. The data is a copy owned by the receiver
// (the platform ring buffer is released on read).
type Packet struct {
	Data []byte
}

// DeviceOptions tune one device open.
type DeviceOptions struct {
	// MTU is the packet size (0 = 1280, the safe tunnel default).
	MTU int

	// RingCapacity is the Wintun ring capacity in bytes
	// (0 = 4 MiB, the upstream default).
	RingCapacity uint32

	// PacketBuffer is the bounded packet-channel capacity
	// (0 = DefaultPacketBuffer).
	PacketBuffer int

	// ReadTimeout bounds one ring wait when the platform API needs a
	// poll deadline (0 = DefaultReadTimeout).
	ReadTimeout time.Duration
}

// Defaults for the device options.
const (
	DefaultMTU          = 1280
	DefaultRingCapacity = 0x400000
	DefaultPacketBuffer = 1024
	DefaultReadTimeout  = 500 * time.Millisecond
)

// ErrUnsupportedPlatform is returned by Open on platforms without a
// first-party TUN device layer (honest refusal, never a fake
// adapter).
var ErrUnsupportedPlatform = errors.New("freecore.tun: first-party TUN device layer is Windows-only in this release")

// WithDefaults applies the option defaults.
func (o DeviceOptions) WithDefaults() DeviceOptions {
	resolved := o

	if resolved.MTU <= 0 {
		resolved.MTU = DefaultMTU
	}

	if resolved.RingCapacity == 0 {
		resolved.RingCapacity = DefaultRingCapacity
	}

	if resolved.PacketBuffer <= 0 {
		resolved.PacketBuffer = DefaultPacketBuffer
	}

	if resolved.ReadTimeout <= 0 {
		resolved.ReadTimeout = DefaultReadTimeout
	}

	return resolved
}

// loopDevice is the platform-neutral skeleton every platform device
// implementation embeds: the bounded channel, the read-loop
// lifecycle and the idempotent close. It carries no packets by
// itself — the platform layer feeds it.
type loopDevice struct {
	identity Identity
	opts     DeviceOptions

	packets chan Packet

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

func newLoopDevice(identity Identity, opts DeviceOptions) loopDevice {
	opts = opts.WithDefaults()

	return loopDevice{
		identity: identity,
		opts:     opts,
		packets:  make(chan Packet, opts.PacketBuffer),
		closed:   make(chan struct{}),
	}
}

// Identity implements Device.
func (d *loopDevice) Identity() Identity { return d.identity }

// Packets implements Device.
func (d *loopDevice) Packets() <-chan Packet { return d.packets }

// Done implements Device.
func (d *loopDevice) Done() <-chan struct{} { return d.closed }

// deliver pushes one inbound packet into the bounded channel,
// honoring the device context. It returns false when the device is
// closed (the platform read loop must exit).
//
// Termination is checked BEFORE the send (deterministically — a
// select with a ready send and a ready termination case picks
// randomly), and re-checked after a successful send so a Close that
// raced the send still stops the producer. The packet channel is
// never closed, so the send itself cannot panic.
func (d *loopDevice) deliver(ctx context.Context, p Packet) bool {
	select {
	case <-d.closed:
		return false
	default:
	}

	select {
	case <-ctx.Done():
		return false
	default:
	}

	select {
	case <-d.closed:
		return false
	case <-ctx.Done():
		return false
	case d.packets <- p:
		select {
		case <-d.closed:
			return false
		default:
		}

		return true
	}
}

// shutdown signals termination exactly once and records the close
// error. The packet channel is intentionally NOT closed (see the
// Device contract).
func (d *loopDevice) shutdown(err error) {
	d.closeOnce.Do(func() {
		if err != nil {
			d.closeErr = fmt.Errorf("freecore.tun: device close: %w", err)
		}

		close(d.closed)
	})
}
