//go:build windows

package tun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

// wintunDevice is the Windows first-party TUN device: a Wintun
// adapter and session owned by the deterministic FreeIran identity,
// with a bounded packet channel fed by one read loop.
//
// License note: golang.zx2c4.com/wintun is MIT-licensed (reviewed
// before adoption); it memory-loads the embedded official wintun.dll
// (no download, no PATH resolution, no disk artifact). Adapter
// creation requires an elevated process and the Wintun driver —
// failures surface as honest errors, never as a fake device.
type wintunDevice struct {
	loopDevice

	adapter  *wintun.Adapter
	session  wintun.Session
	readDone chan struct{}
}

// OpenDevice creates (or reopens) the FreeIran adapter and starts the
// session read loop. The adapter identity is deterministic: the same
// installation always owns the same adapter slot (requested GUID).
func OpenDevice(ctx context.Context, identity Identity, opts DeviceOptions) (Device, error) {
	opts = opts.WithDefaults()

	guid := windows.GUID{
		Data1: binaryLEU32(identity.GUIDBytes[0:4]),
		Data2: binaryLEU16(identity.GUIDBytes[4:6]),
		Data3: binaryLEU16(identity.GUIDBytes[6:8]),
		Data4: [8]byte(identity.GUIDBytes[8:16]),
	}

	adapter, err := wintun.CreateAdapter(identity.AdapterName, identity.TunnelType, &guid)
	if err != nil {
		return nil, fmt.Errorf("freecore.tun: create adapter %q: %w", identity.AdapterName, err)
	}

	session, err := adapter.StartSession(opts.RingCapacity)
	if err != nil {
		_ = adapter.Close()

		return nil, fmt.Errorf("freecore.tun: start session: %w", err)
	}

	device := &wintunDevice{
		loopDevice: newLoopDevice(identity, opts),
		adapter:    adapter,
		session:    session,
		readDone:   make(chan struct{}),
	}

	go device.readLoop(ctx)

	return device, nil
}

// MTU implements Device (address/MTU application itself belongs to
// the Phase 2 activation transaction, not the device layer).
func (d *wintunDevice) MTU() int { return d.opts.MTU }

// LUID returns the adapter's locally unique identifier — the value
// the IP Helper observation seam correlates routes and addresses by.
func (d *wintunDevice) LUID() uint64 { return d.adapter.LUID() }

// readLoop drains the session ring into the bounded packet channel.
// One loop, one goroutine, backpressure through the channel: when
// the buffer is full the loop waits (never drops silently), and
// device close / context cancellation end it deterministically.
func (d *wintunDevice) readLoop(ctx context.Context) {
	defer close(d.readDone)

	event := windows.Handle(d.session.ReadWaitEvent())

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.closed:
			return
		default:
		}

		// Bounded wait so cancellation is observed even without a
		// packet (the ring event alone could sleep indefinitely).
		if err := waitForSingleObject(event, d.opts.ReadTimeout); err != nil {
			// Timeout (or wait failure): loop around and re-check
			// cancellation first.
			continue
		}

		for {
			packet, err := d.session.ReceivePacket()
			if err != nil {
				// ERROR_NO_MORE_ITEMS: the ring is drained for now.
				break
			}

			// Copy out of the ring before release: the packet channel
			// owns its bytes; the ring slot is immediately reusable.
			cp := make([]byte, len(packet))
			copy(cp, packet)

			d.session.ReleaseReceivePacket(packet)

			if !d.deliver(ctx, Packet{Data: cp}) {
				return
			}
		}
	}
}

// WritePacket implements Device.
func (d *wintunDevice) WritePacket(p Packet) error {
	select {
	case <-d.closed:
		return errors.New("freecore.tun: device is closed")
	default:
	}

	if len(p.Data) == 0 {
		return errors.New("freecore.tun: empty packet")
	}

	d.session.SendPacket(p.Data)

	return nil
}

// Close implements Device: end the session, join the read loop, then
// remove the adapter (reverse creation order). Idempotent.
func (d *wintunDevice) Close() error {
	d.shutdown(nil)

	d.session.End()

	<-d.readDone

	if err := d.adapter.Close(); err != nil {
		return fmt.Errorf("freecore.tun: adapter close: %w", err)
	}

	return nil
}

// errWaitTimeout distinguishes "the bounded wait elapsed" from real
// wait failures so the read loop can re-check cancellation.
var errWaitTimeout = errors.New("freecore.tun: ring wait timed out")

// waitForSingleObject wraps the single-handle wait with the bounded
// timeout the read loop needs.
func waitForSingleObject(handle windows.Handle, timeout time.Duration) error {
	status, err := windows.WaitForSingleObject(handle, uint32(timeout.Milliseconds()))
	if err != nil {
		return err
	}

	if status == uint32(windows.WAIT_TIMEOUT) {
		return errWaitTimeout
	}

	return nil
}

// binaryLEU32 reads a little-endian uint32 (GUID Data1 layout).
func binaryLEU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// binaryLEU16 reads a little-endian uint16 (GUID Data2/Data3 layout).
func binaryLEU16(b []byte) uint16 {
	return uint16(b[0]) | uint16(b[1])<<8
}
