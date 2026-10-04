package freecore

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
)

// RouterPolicy is the deterministic configuration of the default
// Router. Phase 2 keeps the model deliberately small — DIRECT, PROXY,
// BLOCK over an ordered rule set — while making the decision real:
// every flow carries a decision object that names the action, the
// outbound, the resolver choice and the matching rule.
type RouterPolicy struct {
	// PrivateDirect allows local/private destinations to leave without
	// the remote (the System Proxy behavior every OS expects: LAN and
	// loopback stay local). When false, even private addresses go
	// through the remote — the strict TUN posture.
	PrivateDirect bool

	// BlockCIDRs refuses destinations in these networks outright
	// (fail-closed; the flow never reaches any network). Empty = none.
	BlockCIDRs []netip.Prefix

	// DirectCIDRs sends destinations in these networks direct (when
	// PrivateDirect alone is not granular enough). Evaluated after the
	// block list. Empty = none.
	DirectCIDRs []netip.Prefix

	// DefaultOutbound is the outbound name for proxied flows (the
	// session's configured remote kind).
	DefaultOutbound string
}

// defaultRouter is the Phase 2 routing authority: ONE deterministic
// evaluator that the local inbounds, the TUN flow path and the
// diagnostics probes all call. Rules are evaluated in a fixed order;
// the first match wins; no match falls to the configured default
// policy. No protocol implementation hides a decision.
type defaultRouter struct {
	route      Route
	policy     RouterPolicy
	decisions  RouterDecision // precomputed proxy decision (immutable)
	onceInit   sync.Once
	blockNets  []netip.Prefix
	directNets []netip.Prefix
}

// DefaultRouter builds the default Router for one engine run. The
// default policy preserves the v0.13.1 first-party contract: ALL
// traffic flows through the configured remote (the operator's bypass
// surface lives above the engine). Private/direct rules become active
// only through an explicit RouterPolicy.
func DefaultRouter(route Route) Router {
	return DefaultRouterWithPolicy(route, RouterPolicy{
		PrivateDirect:   false,
		DefaultOutbound: string(route.Outbound),
	})
}

// DefaultRouterWithPolicy builds the default Router with an explicit
// policy (the TUN dataplane passes PrivateDirect=false so a flow never
// silently leaves without the remote unless a rule says so).
func DefaultRouterWithPolicy(route Route, policy RouterPolicy) Router {
	r := &defaultRouter{route: route, policy: policy}

	if policy.DefaultOutbound == "" {
		r.policy.DefaultOutbound = string(route.Outbound)
	}

	return r
}

// Decide implements Router. Deterministic, side-effect free.
func (r *defaultRouter) Decide(_ context.Context, target string) RouterDecision {
	r.onceInit.Do(r.init)

	host, _, err := net.SplitHostPort(strings.TrimSpace(target))
	if err != nil {
		// A target that cannot carry a port is malformed input, not a
		// routing puzzle: refuse it (fail-closed beats guessing).
		return RouterDecision{
			Action:   ActionBlock,
			Outbound: r.policy.DefaultOutbound,
			Reason:   "destination is not a valid host:port form",
			RuleID:   "malformed-target",
		}
	}

	addr, isLiteral := literalAddr(host)

	// 1. Block list (literal or resolvable-from-text check happens for
	// literals; domain block matching is a Phase 3 capability and is
	// deliberately NOT approximated here — no fuzzy host matching).
	if isLiteral {
		for _, p := range r.blockNets {
			if p.Contains(addr) {
				return RouterDecision{
					Action:   ActionBlock,
					Outbound: r.policy.DefaultOutbound,
					Reason:   fmt.Sprintf("destination %s is inside blocked network %s", addr, p),
					RuleID:   "block-list",
				}
			}
		}
	}

	// 2. Loopback/link-local/private → direct when the policy allows.
	// Private ranges are the OS's own neighborhood (LAN printers, host
	// services, the local gateway); a proxy remote must not see them
	// unless the operator demanded strictness.
	if isLiteral && r.policy.PrivateDirect && isLocalPrivate(addr) {
		return RouterDecision{
			Action:         ActionDirect,
			Outbound:       string(OutboundDirect),
			ResolverChoice: "none",
			Reason:         fmt.Sprintf("destination %s is local/private (policy: local stays local)", addr),
			RuleID:         "private-direct",
		}
	}

	// 3. Explicit direct list.
	if isLiteral {
		for _, p := range r.directNets {
			if p.Contains(addr) {
				return RouterDecision{
					Action:         ActionDirect,
					Outbound:       string(OutboundDirect),
					ResolverChoice: "engine",
					Reason:         fmt.Sprintf("destination %s is inside direct network %s", addr, p),
					RuleID:         "direct-list",
				}
			}
		}
	}

	// 4. Default policy: everything else goes through the configured
	// remote. Domains resolve remotely (the domain is preserved through
	// the first-party outbound — the DNS request never leaks to the
	// host network through the TUN path).
	return r.decisions
}

// init precomputes the immutable decisions and networks.
func (r *defaultRouter) init() {
	r.blockNets = append([]netip.Prefix(nil), r.policy.BlockCIDRs...)
	r.directNets = append([]netip.Prefix(nil), r.policy.DirectCIDRs...)

	r.decisions = RouterDecision{
		Action:         ActionProxy,
		Outbound:       r.policy.DefaultOutbound,
		ResolverChoice: "remote",
		Reason: fmt.Sprintf("default policy: %s traffic flows through the configured %s remote %s",
			r.route.Outbound, r.route.Outbound, r.route.Endpoint),
		RuleID: "default-policy",
	}
}

// literalAddr parses host as an IP literal (v4 or v6). The ok result
// distinguishes "parsed" from "domain that merely looks like garbage".
func literalAddr(host string) (netip.Addr, bool) {
	if strings.Contains(host, "%") {
		// Zone identifiers (link-local) are host-local by definition;
		// trim the zone so fe80::1%eth0 matches link-local rules.
		if i := strings.IndexByte(host, '%'); i > 0 {
			host = host[:i]
		}
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}

	return addr.Unmap(), true
}

// isLocalPrivate reports whether addr belongs to the machine's own
// neighborhood: loopback, link-local unicast/multicast, RFC 1918 and
// RFC 4193, the IPv4/6 unspecified and broadcast forms. IPv4-mapped
// IPv6 inputs arrive already unmapped.
func isLocalPrivate(addr netip.Addr) bool {
	return addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() ||
		addr.IsPrivate() ||
		addr.IsUnspecified()
}
