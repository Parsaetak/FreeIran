//go:build windows

// tun_routes_windows.go: the native Windows route/owner-table
// collectors behind the v0.11.4 TUN activation gate. Both use the
// Windows IP Helper API (iphlpapi.dll) through lazy syscalls —
// READ-ONLY queries with bounded, explicitly sized buffers; no shell
// command, no netsh, no route.exe, no PowerShell, ever.
//
//   - GetIpForwardTable      — the IPv4 forwarding table
//     (MIB_IPFORWARDTABLE: dwNumEntries + MIB_IPFORWARDROW rows of
//     14 DWORDs each). Legacy on paper, but a stable, simple,
//     byte-order-explicit surface that every Windows release still
//     exports; sufficient for the covering-route verdict.
//   - GetExtendedTcpTable    — the TCP owner table
//     (MIB_TCPTABLE_OWNER_PID: rows of MIB_TCPROW_OWNER_PID — state,
//     local addr/port, remote addr/port, owning PID), which carries
//     the per-socket local SOURCE address the upstream-pinning
//     verdict reasons over.
package tunnel

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// mibIPForwardRow mirrors the 14-DWORD MIB_IPFORWARDROW layout
// (iphlpapi.h). All fields are DWORD-sized, so the struct is
// alignment-trivial.
type mibIPForwardRow struct {
	Dest      uint32 // dwForwardDest (network byte order)
	Mask      uint32 // dwForwardMask (network byte order)
	Policy    uint32 // dwForwardPolicy
	NextHop   uint32 // dwForwardNextHop (network byte order)
	IfIndex   uint32 // dwForwardIfIndex
	Type      uint32 // dwForwardType
	Proto     uint32 // dwForwardProto
	Age       uint32 // dwForwardAge
	NextHopAS uint32 // dwForwardNextHopAS
	Metric1   uint32 // dwForwardMetric1
	Metric2   uint32 // dwForwardMetric2
	Metric3   uint32 // dwForwardMetric3
	Metric4   uint32 // dwForwardMetric4
	Metric5   uint32 // dwForwardMetric5
}

// mibTCPRowOwnerPID mirrors MIB_TCPROW_OWNER_PID (6 DWORDs).
type mibTCPRowOwnerPID struct {
	State      uint32 // TCP state enum
	LocalAddr  uint32 // network byte order
	LocalPort  uint32 // network byte order in the low 16 bits
	RemoteAddr uint32 // network byte order
	RemotePort uint32 // network byte order in the low 16 bits
	PID        uint32 // owning process id
}

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetIpForwardTable   = iphlpapi.NewProc("GetIpForwardTable")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

// bytesToIPv4 converts a network-byte-order DWORD (as stored by the
// IP Helper API — the address octets sit at increasing byte
// addresses, so the little-endian DWORD read places the FIRST octet
// in the LOW byte) to the [4]byte host representation used by the
// observedRoute/observedOwnerSocket model (element 0 is the most
// significant address octet).
func bytesToIPv4(v uint32) [4]byte {
	return [4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

// collectWindowsForwardTable snapshots the IPv4 forwarding table.
// Documented two-call pattern: an initial minimal buffer, resize on
// ERROR_INSUFFICIENT_BUFFER (122), one retry; the buffer is bounded
// by the reported table size.
func collectWindowsForwardTable() ([]observedRoute, error) {
	rowSize := uint32(unsafe.Sizeof(mibIPForwardRow{}))
	size := uint32(4) + rowSize // header + one row to start

	var (
		buf []byte
		ret uintptr
	)

	for attempt := 0; attempt < 2; attempt++ {
		buf = make([]byte, size)

		ret, _, _ = procGetIpForwardTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
		if ret == 0 {
			break
		}

		if ret != 122 { // ERROR_INSUFFICIENT_BUFFER
			return nil, windows.Errno(ret)
		}

		if size > 4<<20 { // bounded: refuse pathological tables
			return nil, windows.Errno(122)
		}
	}

	if ret != 0 {
		return nil, windows.Errno(ret)
	}

	count := *(*uint32)(unsafe.Pointer(&buf[0]))

	avail := (uint32(len(buf)) - 4) / rowSize
	if count > avail {
		count = avail
	}

	routes := make([]observedRoute, 0, count)

	for i := uint32(0); i < count; i++ {
		row := (*mibIPForwardRow)(unsafe.Pointer(&buf[4+i*rowSize]))

		routes = append(routes, observedRoute{
			Dest:    bytesToIPv4(row.Dest),
			Mask:    bytesToIPv4(row.Mask),
			IfIndex: row.IfIndex,
			Metric:  row.Metric1,
		})
	}

	return routes, nil
}

// collectWindowsOwnerTCPSockets snapshots the TCP owner table
// (ALL states — an upstream socket that just served the probe is at
// minimum in TIME_WAIT, and pooled upstreams are ESTABLISHED, so the
// single snapshot is sufficient without polling).
func collectWindowsOwnerTCPSockets() ([]observedOwnerSocket, error) {
	const (
		tcpTableOwnerPIDAll = 5 // TCP_TABLE_OWNER_PID_ALL
		afInet              = 2
	)

	var size uint32

	// Size probe with a NULL table: ERROR_INSUFFICIENT_BUFFER (122)
	// is the expected return.
	ret, _, _ := procGetExtendedTcpTable.Call(
		uintptr(unsafe.Pointer(nil)),
		uintptr(unsafe.Pointer(&size)),
		0, uintptr(afInet), uintptr(tcpTableOwnerPIDAll), 0,
	)
	if ret != 122 && ret != 0 {
		return nil, windows.Errno(ret)
	}

	if size == 0 {
		return nil, nil // no sockets at all
	}

	if size > 8<<20 { // bounded: refuse pathological tables
		return nil, windows.Errno(122)
	}

	buf := make([]byte, size)

	ret, _, _ = procGetExtendedTcpTable.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		0, uintptr(afInet), uintptr(tcpTableOwnerPIDAll), 0,
	)
	if ret != 0 {
		return nil, windows.Errno(ret)
	}

	count := *(*uint32)(unsafe.Pointer(&buf[0]))

	const rowSize = uint32(unsafe.Sizeof(mibTCPRowOwnerPID{}))
	avail := (size - 4) / rowSize
	if count > avail {
		count = avail
	}

	sockets := make([]observedOwnerSocket, 0, count)

	for i := uint32(0); i < count; i++ {
		row := (*mibTCPRowOwnerPID)(unsafe.Pointer(&buf[4+i*rowSize]))

		sockets = append(sockets, observedOwnerSocket{
			PID:       row.PID,
			LocalAddr: bytesToIPv4(row.LocalAddr),
			// dwLocalPort carries the port in network byte order in the
			// low 16 bits: swap the two port bytes into host order.
			LocalPort: uint16((row.LocalPort&0xFF)<<8 | (row.LocalPort>>8)&0xFF),
		})
	}

	return sockets, nil
}

func init() {
	collectForwardRoutes = collectWindowsForwardTable
	collectOwnerTCPSockets = collectWindowsOwnerTCPSockets
}
