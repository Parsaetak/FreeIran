//go:build windows

package tun

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Observe collects the READ-ONLY IP Helper facts the first-party TUN
// control plane reasons over: interfaces with their unicast
// addresses (GetAdaptersAddresses) and the IPv4 forwarding table
// (GetIpForwardTable — the same stable, byte-order-explicit surface
// engine/tunnel's activation gate uses). No mutation, no shell, no
// netsh/route.exe/PowerShell — ever.
func Observe() (Observation, error) {
	interfaces, err := observeInterfaces()
	if err != nil {
		return Observation{}, fmt.Errorf("freecore.tun: observe interfaces: %w", err)
	}

	routes, err := observeRoutes()
	if err != nil {
		return Observation{}, fmt.Errorf("freecore.tun: observe routes: %w", err)
	}

	return Observation{
		Interfaces:  interfaces,
		Routes:      routes,
		CollectedAt: time.Now().UTC(),
	}, nil
}

// observeInterfaces walks GetAdaptersAddresses (bounded buffer with
// the documented size-retry).
func observeInterfaces() ([]InterfaceFact, error) {
	const (
		flags = windows.GAA_FLAG_INCLUDE_ALL_INTERFACES |
			windows.GAA_FLAG_SKIP_ANYCAST |
			windows.GAA_FLAG_SKIP_MULTICAST |
			windows.GAA_FLAG_SKIP_DNS_SERVER
		initialSize = 16 * 1024
		maxAttempts = 4
	)

	var (
		buffer  []byte
		size    = uint32(initialSize)
		lastErr error
	)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		buffer = make([]byte, size)

		lastErr = windows.GetAdaptersAddresses(
			windows.AF_UNSPEC, flags, 0,
			(*windows.IpAdapterAddresses)(unsafe.Pointer(&buffer[0])), &size)
		if lastErr == nil {
			break
		}

		if !isBufferOverflow(lastErr) {
			return nil, lastErr
		}

		// The call reports the required size; retry with it.
	}

	if lastErr != nil {
		return nil, lastErr
	}

	var out []InterfaceFact

	adapter := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buffer[0]))
	for adapter != nil {
		fact := InterfaceFact{
			Name:    windows.UTF16PtrToString(adapter.FriendlyName),
			Index:   adapter.IfIndex,
			Running: adapter.OperStatus == windows.IfOperStatusUp,
		}

		// Unicast addresses, both families, through the typed helper.
		for addr := adapter.FirstUnicastAddress; addr != nil; addr = addr.Next {
			ip := addr.Address.IP()
			if ip == nil {
				continue
			}

			a, ok := netip.AddrFromSlice(ip)
			if !ok || !a.IsValid() {
				continue
			}

			// Normalize v4-in-v6 mappings to plain v4.
			if a.Is4In6() {
				a = a.Unmap()
			}

			fact.Addresses = append(fact.Addresses, netip.PrefixFrom(a, int(addr.OnLinkPrefixLength)))
		}

		out = append(out, fact)

		adapter = adapter.Next
	}

	return out, nil
}

// isBufferOverflow matches ERROR_BUFFER_OVERFLOW (the documented
// size-retry signal of GetAdaptersAddresses).
func isBufferOverflow(err error) bool {
	return errors.Is(err, windows.ERROR_BUFFER_OVERFLOW)
}

// mibIPForwardRowT mirrors the 14-DWORD MIB_IPFORWARDROW layout
// (iphlpapi.h) — the same shape engine/tunnel's route gate parses.
type mibIPForwardRowT struct {
	Dest      uint32
	Mask      uint32
	Policy    uint32
	NextHop   uint32
	IfIndex   uint32
	Type      uint32
	Proto     uint32
	Age       uint32
	NextHopAS uint32
	Metric1   uint32
	Metric2   uint32
	Metric3   uint32
	Metric4   uint32
	Metric5   uint32
}

var (
	iphlpapiDLL            = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetIpForwardTableT = iphlpapiDLL.NewProc("GetIpForwardTable")
)

// observeRoutes reads the IPv4 forwarding table.
func observeRoutes() ([]RouteFact, error) {
	var (
		buffer []byte
		size   = uint32(4096)
	)

	for attempt := 0; attempt < 4; attempt++ {
		buffer = make([]byte, size)

		ret, _, _ := procGetIpForwardTableT.Call(
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if ret == 0 {
			break
		}

		if ret != 111 /* ERROR_INSUFFICIENT_BUFFER */ {
			return nil, fmt.Errorf("GetIpForwardTable: win32 error %d", ret)
		}

		buffer = nil
	}

	if len(buffer) < 4 {
		return nil, fmt.Errorf("GetIpForwardTable: truncated table header")
	}

	count := *(*uint32)(unsafe.Pointer(&buffer[0]))

	const rowSize = unsafe.Sizeof(mibIPForwardRowT{})

	if uint64(4)+uint64(count)*uint64(rowSize) > uint64(len(buffer)) {
		return nil, fmt.Errorf("GetIpForwardTable: table overruns buffer")
	}

	out := make([]RouteFact, 0, count)

	for i := range count {
		row := (*mibIPForwardRowT)(unsafe.Pointer(&buffer[4+uintptr(i)*rowSize]))

		dest := uint32ToIPv4(row.Dest)
		mask := uint32ToIPv4(row.Mask)

		ones, bits := maskPrefixLen(mask)
		if bits == 0 {
			continue
		}

		prefix, err := dest.Prefix(ones)
		if err != nil {
			continue
		}

		out = append(out, RouteFact{
			Prefix:         prefix,
			InterfaceIndex: row.IfIndex,
			Gateway:        uint32ToIPv4(row.NextHop),
		})
	}

	return out, nil
}

// uint32ToIPv4 converts a network-byte-order DWORD to an address.
func uint32ToIPv4(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}

// maskPrefixLen converts a mask address to (ones, bits).
func maskPrefixLen(mask netip.Addr) (int, int) {
	if !mask.Is4() {
		return 0, 0
	}

	ones := 0

	for _, b := range mask.As4() {
		switch {
		case b == 0xFF:
			ones += 8
		case b == 0x00:
			// remaining bits must also be zero for a contiguous mask
			return ones, 32
		default:
			return ones, 32
		}
	}

	return ones, 32
}
