package netstack

import (
	"errors"
	"fmt"
	"net/netip"
)

// Defaults applied by New when the corresponding Config field is zero.
// DefaultMTU is deliberately the tun package's default: one honest
// tunnel MTU for the whole dataplane, not two that can drift apart.
const (
	DefaultMaxFlows  = 256
	DefaultQueueSize = 256
)

// maxIPPacket is the largest IP packet that can exist on the wire
// (the IP total-length field is 16 bits). A Config or device MTU
// beyond it cannot carry a valid packet and is refused.
const maxIPPacket = 65535

// Config sizes one stack instance. Every field is validated by New —
// a config that cannot be honored exactly fails the constructor
// (fail-closed), it is never silently clamped into something the
// operator did not ask for.
type Config struct {
	// MTU is the packet size for the served device (>0; 0 = adopt the
	// device's negotiated MTU). An explicit value that disagrees with
	// device.MTU() is an error: the stack enforces this MTU on ingress
	// (oversize packets are Malformed) and configures the link endpoint
	// with it, so a lie here would corrupt both bounds at once.
	MTU int

	// Addr4 is the TUN IPv4 interface address (required).
	Addr4 netip.Addr

	// Prefix4 is the local network the interface address lives in,
	// e.g. a 198.18.0.0/30 style point-to-point lan (required). Addr4
	// must be contained in it; the prefix length — not the base
	// address — is what the interface address is configured with.
	Prefix4 netip.Prefix

	// Addr6 is the optional TUN IPv6 interface address.
	Addr6 netip.Addr

	// Prefix6 is the optional IPv6 local network. Required when Addr6
	// is set; an error when set without Addr6 (a network without an
	// interface address is a misconfiguration, not a default).
	Prefix6 netip.Prefix

	// MaxFlows bounds concurrent open flows, TCP and UDP together
	// (0 = DefaultMaxFlows). Beyond the cap a TCP connection request is
	// answered with RST and a UDP datagram is counted and dropped.
	MaxFlows int

	// QueueSize is the link channel depth in packets (0 =
	// DefaultQueueSize). It bounds the memory the stack's egress path
	// can pin.
	QueueSize int
}

// resolvedConfig is the validated internal form of Config: every field
// carries a usable value, so the packet path never re-validates.
type resolvedConfig struct {
	mtu       int
	addr4     netip.Addr
	prefix4   netip.Prefix
	addr6     netip.Addr // zero value when IPv6 is not configured
	prefix6   netip.Prefix
	maxFlows  int
	queueSize int
}

// resolve validates Config against the device it will serve and
// applies the documented defaults.
func (c Config) resolve(deviceMTU int) (resolvedConfig, error) {
	if deviceMTU <= 0 {
		return resolvedConfig{}, errors.New(Subsystem + ": device MTU is not negotiated")
	}

	mtu := c.MTU
	switch {
	case mtu < 0:
		return resolvedConfig{}, fmt.Errorf("%s: negative MTU %d", Subsystem, c.MTU)
	case mtu == 0:
		// The device owns the wire truth; an unspecified MTU adopts it.
		mtu = deviceMTU
	case mtu > maxIPPacket:
		return resolvedConfig{}, fmt.Errorf("%s: MTU %d exceeds the maximum IP packet size %d", Subsystem, c.MTU, maxIPPacket)
	case mtu != deviceMTU:
		// An explicit MTU that disagrees with the device is a config lie:
		// ingress enforcement and link configuration would disagree with
		// the wire. Refuse instead of silently picking a side.
		return resolvedConfig{}, fmt.Errorf("%s: configured MTU %d disagrees with device MTU %d", Subsystem, c.MTU, deviceMTU)
	}

	addr4, prefix4, err := resolveFamily(c.Addr4, c.Prefix4, 4)
	if err != nil {
		return resolvedConfig{}, err
	}

	addr6, prefix6, err := resolveFamily(c.Addr6, c.Prefix6, 6)
	if err != nil {
		return resolvedConfig{}, err
	}

	if !addr6.IsValid() && prefix6.IsValid() {
		return resolvedConfig{}, errors.New(Subsystem + ": Prefix6 set without Addr6")
	}

	if c.MaxFlows < 0 {
		return resolvedConfig{}, fmt.Errorf("%s: negative MaxFlows %d", Subsystem, c.MaxFlows)
	}

	if c.QueueSize < 0 {
		return resolvedConfig{}, fmt.Errorf("%s: negative QueueSize %d", Subsystem, c.QueueSize)
	}

	maxFlows := c.MaxFlows
	if maxFlows == 0 {
		maxFlows = DefaultMaxFlows
	}

	queueSize := c.QueueSize
	if queueSize == 0 {
		queueSize = DefaultQueueSize
	}

	return resolvedConfig{
		mtu:       mtu,
		addr4:     addr4,
		prefix4:   prefix4,
		addr6:     addr6,
		prefix6:   prefix6,
		maxFlows:  maxFlows,
		queueSize: queueSize,
	}, nil
}

// resolveFamily validates one address/prefix pair. The prefix is
// required whenever the address is set, must belong to the same family
// and must contain the address — an interface address outside its own
// network would make the stack advertise a route it cannot honor.
func resolveFamily(addr netip.Addr, prefix netip.Prefix, family int) (netip.Addr, netip.Prefix, error) {
	name := "4"

	if family == 6 {
		name = "6"
	}

	if !addr.IsValid() {
		if family == 4 {
			return netip.Addr{}, netip.Prefix{}, errors.New(Subsystem + ": Addr4 is required")
		}

		// IPv6 is optional: an invalid Addr6 simply means "off".
		return netip.Addr{}, netip.Prefix{}, nil
	}

	if family == 4 && !addr.Is4() {
		return netip.Addr{}, netip.Prefix{}, errors.New(Subsystem + ": Addr4 must be a plain IPv4 address")
	}

	if family == 6 && addr.Is4In6() {
		return netip.Addr{}, netip.Prefix{}, errors.New(Subsystem + ": Addr6 must be a plain IPv6 address, not an IPv4-mapped one")
	}

	if addr.IsUnspecified() {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf(Subsystem+": Addr%s must not be the unspecified address", name)
	}

	if addr.IsMulticast() {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf(Subsystem+": Addr%s must not be multicast", name)
	}

	if !prefix.IsValid() {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf(Subsystem+": Prefix%s is required when Addr%s is set", name, name)
	}

	if family == 4 && !prefix.Addr().Is4() || family == 6 && prefix.Addr().Is4() {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf(Subsystem+": Prefix%s family does not match Addr%s", name, name)
	}

	if !prefix.Contains(addr) {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf(Subsystem+": Addr%s %s is outside Prefix%s %s", name, addr, name, prefix)
	}

	return addr, prefix, nil
}
