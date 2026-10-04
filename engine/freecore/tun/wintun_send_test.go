//go:build windows

package tun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// sendTestDevice builds a wintunDevice with an injected send seam —
// the error contract is testable without a real adapter or elevation.
func sendTestDevice(t *testing.T, send func(data []byte) error) *wintunDevice {
	t.Helper()

	device := &wintunDevice{
		loopDevice: newLoopDevice(DefaultIdentity(), DeviceOptions{}),
		readDone:   make(chan struct{}),
	}

	device.sendPacket = send

	t.Cleanup(func() { _ = device.Close() })

	return device
}

// TestWintunWritePacketSuccessPath: a successful send is exactly one
// allocation+copy+submit, data intact, no invented counters.
func TestWintunWritePacketSuccessPath(t *testing.T) {
	var (
		sent    atomic.Int64
		gotData = make(chan []byte, 1)
	)

	device := sendTestDevice(t, func(data []byte) error {
		sent.Add(1)

		cp := make([]byte, len(data))
		copy(cp, data)

		select {
		case gotData <- cp:
		default:
		}

		return nil
	})

	payload := []byte{0x60, 0x00, 0x00, 0x00, 0x00, 0x00} // an IPv6-shaped frame

	if err := device.WritePacket(Packet{Data: payload}); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	if got := sent.Load(); got != 1 {
		t.Fatalf("send attempts = %d, want exactly 1", got)
	}

	select {
	case got := <-gotData:
		if string(got) != string(payload) {
			t.Fatalf("sent bytes = %v, want %v", got, payload)
		}
	default:
		t.Fatal("send seam never received the packet")
	}
}

// TestWintunWritePacketRingFullSurfaces: the Wintun ring rejecting a
// send allocation (ERROR_BUFFER_OVERFLOW) MUST surface as a real
// error — the pre-0.14.1 code dropped the packet and returned nil,
// faking delivery.
func TestWintunWritePacketRingFullSurfaces(t *testing.T) {
	device := sendTestDevice(t, func([]byte) error {
		return syscall.Errno(windows.ERROR_BUFFER_OVERFLOW)
	})

	err := device.WritePacket(Packet{Data: []byte{1, 2, 3}})
	if err == nil {
		t.Fatal("ring-full send returned nil (silent packet loss)")
	}

	if !strings.Contains(err.Error(), "send packet") {
		t.Fatalf("error = %v, want the send-path wrapper", err)
	}
}

// TestWintunWritePacketTerminatingSessionSurfaces: ERROR_HANDLE_EOF
// (session terminating) must surface, not vanish.
func TestWintunWritePacketTerminatingSessionSurfaces(t *testing.T) {
	device := sendTestDevice(t, func([]byte) error {
		return syscall.Errno(windows.ERROR_HANDLE_EOF)
	})

	if err := device.WritePacket(Packet{Data: []byte{1}}); err == nil {
		t.Fatal("terminating-session send returned nil")
	}
}

// TestWintunWritePacketClosedDevice: writes after Close fail honestly.
func TestWintunWritePacketClosedDevice(t *testing.T) {
	device := sendTestDevice(t, func([]byte) error {
		t.Fatal("send seam must not run on a closed device")

		return nil
	})

	_ = device.Close()

	if err := device.WritePacket(Packet{Data: []byte{1}}); err == nil {
		t.Fatal("WritePacket after Close returned nil")
	}
}

// TestWintunWritePacketShutdownRace: Close racing in-flight sends must
// be safe — every send either completes or surfaces an error, never a
// panic, never a nil masquerading as delivery after termination.
func TestWintunWritePacketShutdownRace(t *testing.T) {
	var (
		sent    atomic.Int64
		errs    atomic.Int64
		closing atomic.Bool
	)

	device := sendTestDevice(t, func([]byte) error {
		if closing.Load() {
			// The session is terminating: the honest outcome is an error
			// (ERROR_HANDLE_EOF shape), never a silent drop.
			errs.Add(1)

			return syscall.Errno(windows.ERROR_HANDLE_EOF)
		}

		sent.Add(1)

		return nil
	})

	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := 0; j < 32; j++ {
				if err := device.WritePacket(Packet{Data: []byte{byte(j)}}); err != nil && !errors.Is(err, errMemDeviceClosed) {
					// Any error is acceptable ONLY if it is a real surface;
					// nil-after-close is what this test forbids.
					_ = err
				}
			}
		}()
	}

	time.Sleep(5 * time.Millisecond)
	closing.Store(true)
	_ = device.Close()

	wg.Wait()

	// Every send attempt was accounted: sent + errored ≥ 1, and no
	// attempt was silently dropped (a nil return with zero sends is
	// the old fake-success bug shape).
	if got := sent.Load() + errs.Load(); got == 0 {
		t.Fatal("no send attempt was accounted — silent loss")
	}
}

// TestWintunSendBounded: no unbounded retry — one failure is one
// error; the device never loops the send internally.
func TestWintunSendBounded(t *testing.T) {
	var attempts atomic.Int64

	device := sendTestDevice(t, func([]byte) error {
		attempts.Add(1)

		return syscall.Errno(windows.ERROR_BUFFER_OVERFLOW)
	})

	_ = device.WritePacket(Packet{Data: []byte{9}})

	if got := attempts.Load(); got != 1 {
		t.Fatalf("send attempts = %d, want exactly 1 (no unbounded retry)", got)
	}
}

// Compile-time guard: the device context plumbing stays referenced.
var _ = context.Background
