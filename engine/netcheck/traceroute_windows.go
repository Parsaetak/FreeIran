//go:build windows

// traceroute_windows.go — the Windows hop walker: the native IP
// Helper ICMP API (iphlpapi.dll: IcmpCreateFile / IcmpSendEcho /
// IcmpCloseHandle).
//
// Design notes (v0.12.1 §10):
//
//   - The previous Windows path opened a raw ip4:icmp socket, which
//     Windows reserves for Administrator processes — the tool then
//     reported "failed/unreachable" for what was really a privilege
//     gap. IcmpSendEcho performs user-mode ICMP echo with per-probe
//     TTL (IP_OPTION_INFORMATION.Ttl): time-exceeded replies surface
//     as IP_TTL_EXPIRED_TRANSIT with the router's address, echo
//     replies as IP_SUCCESS — a working TTL walk WITHOUT elevation,
//     WITHOUT tracert.exe, WITHOUT route.exe, WITHOUT PowerShell and
//     WITHOUT shell parsing. This matches FreeIran's security
//     philosophy: native, bounded, user-mode APIs only.
//   - Every probe is bounded (per-probe timeout), the walk is bounded
//     (max 20 hops × 2 probes, walkTTLs) and no retries are
//     unbounded. Cancellation is honoured between probes.
//   - Handles are closed deterministically (Close + defer in the
//     walk loop's owner).
package netcheck

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi         = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile  = modiphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = modiphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho    = modiphlpapi.NewProc("IcmpSendEcho")
)

// IP_STATUS codes returned in ICMP_ECHO_REPLY.Status (iphlpapi.h).
const (
	ipSuccess              = 0
	ipDestNetUnreachable   = 11002
	ipDestHostUnreachable  = 11003
	ipDestProtoUnreachable = 11004
	ipDestPortUnreachable  = 11005
	ipReqTimedOut          = 11010
	ipTTLExpiredTransit    = 11013
	ipTTLExpiredReassem    = 11014
	ipDestNoRoute          = 11048
)

// ipOptionInformation mirrors the Windows struct (x64 and x86: the
// pointer field forces the same trailing padding the C ABI uses).
type ipOptionInformation struct {
	Ttl         uint8
	Tos         uint8
	Flags       uint8
	OptionsSize uint8
	OptionsData *byte
}

// icmpEchoReply mirrors ICMP_ECHO_REPLY for the fields this walker
// reads. Layout verified against winping/IPHLPAPI on x64 and x86.
type icmpEchoReply struct {
	Address       uint32
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          *byte
	Options       ipOptionInformation
}

// icmpAPIWalker is the Windows hopWalker implementation.
type icmpAPIWalker struct {
	handle windows.Handle
}

// newHopWalker opens the platform ICMP walker. IcmpCreateFile
// requires no elevation; a refusal here is genuinely unusual and is
// surfaced as the honest "unsupported" report.
func newHopWalker(ctx context.Context) (hopWalker, error) {
	_ = ctx // bounded per-probe and by the caller context

	r1, _, _ := procIcmpCreateFile.Call()

	if r1 == 0 || r1 == uintptr(windows.InvalidHandle) {
		return nil, fmt.Errorf("IcmpCreateFile refused the ICMP handle")
	}

	return &icmpAPIWalker{handle: windows.Handle(r1)}, nil
}

// probe sends ONE echo request with the given TTL through
// IcmpSendEcho and classifies the reply.
//
// Blocking call: IcmpSendEcho waits up to icmpProbeWindowMS for the
// answer. The walk loop checks ctx between probes, so cancellation
// lands within one probe window — bounded, never unbounded.
func (w *icmpAPIWalker) probe(ctx context.Context, target net.IP, echoID uint16, ttl int) (hopIP string, replied bool, err error) {
	_ = echoID // the IP Helper matches replies itself (no raw ID filter)

	four := target.To4()
	if four == nil {
		return "", false, fmt.Errorf("IPv6 target is not supported by the Windows ICMP walker")
	}

	payload := []byte("freeiran-traceroute-probe")

	opts := ipOptionInformation{Ttl: uint8(ttl)}

	replySize := uint32(unsafe.Sizeof(icmpEchoReply{})) + uint32(len(payload)) + 8
	replyBuf := make([]byte, replySize)

	dest := binary.BigEndian.Uint32(four)

	replies, _, _ := procIcmpSendEcho.Call(
		uintptr(w.handle),
		uintptr(dest),
		uintptr(unsafe.Pointer(&payload[0])),
		uintptr(len(payload)),
		uintptr(unsafe.Pointer(&opts)),
		uintptr(unsafe.Pointer(&replyBuf[0])),
		uintptr(replySize),
		uintptr(icmpProbeWindowMS),
	)

	if replies == 0 {
		// No reply inside the window: silence is not fabricated into
		// a verdict — the walk loop decides after its retry budget.
		return "", false, nil
	}

	if uintptr(unsafe.Sizeof(icmpEchoReply{})) > uintptr(len(replyBuf)) {
		return "", false, fmt.Errorf("ICMP reply buffer too small")
	}

	reply := (*icmpEchoReply)(unsafe.Pointer(&replyBuf[0]))

	switch reply.Status {
	case ipSuccess:
		// The target itself answered the echo.
		addr := reply.Address

		return ipv4String(addr), true, nil
	case ipTTLExpiredTransit, ipTTLExpiredReassem:
		// An intermediate router returned time-exceeded: this is the
		// hop evidence the walk collects.
		return ipv4String(reply.Address), false, nil
	case ipReqTimedOut:
		return "", false, nil
	case ipDestNetUnreachable, ipDestHostUnreachable, ipDestProtoUnreachable,
		ipDestPortUnreachable, ipDestNoRoute:
		// The path EXPLICITLY reported the destination unreachable.
		// The error text drives the tool's unreachable classification.
		return "", false, fmt.Errorf("destination unreachable (IP_STATUS %d)", reply.Status)
	default:
		return "", false, fmt.Errorf("ICMP probe failed with IP_STATUS %d", reply.Status)
	}
}

// Close releases the ICMP handle.
func (w *icmpAPIWalker) Close() error {
	if w.handle == 0 || w.handle == windows.InvalidHandle {
		return nil
	}

	_, _, _ = procIcmpCloseHandle.Call(uintptr(w.handle))

	w.handle = 0

	return nil
}

// ipv4String renders a network-byte-order IPv4 address.
func ipv4String(addr uint32) string {
	var b [4]byte

	binary.BigEndian.PutUint32(b[:], addr)

	return net.IP(b[:]).String()
}

// icmpProbeWindowMS bounds one IcmpSendEcho wait (matching the Unix
// walker's 1s answer window).
const icmpProbeWindowMS = 1000
