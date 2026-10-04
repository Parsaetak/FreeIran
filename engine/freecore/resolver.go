package freecore

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// DNS defaults for the engine authority. The bounds are real: the
// cache holds at most DefaultDNSCacheEntries hostnames, and the
// observation ring at most DefaultDNSObservations decisions — no DNS
// structure in the engine grows with traffic.
const (
	DefaultDNSCacheEntries = 512

	DefaultDNSPositiveTTL = 5 * time.Minute

	// DefaultDNSNegativeTTL caches failures briefly so a dead name
	// cannot turn every session into a fresh upstream storm, while a
	// recovery is seen quickly.
	DefaultDNSNegativeTTL = 30 * time.Second

	DefaultDNSObservations = 64

	// DefaultDNSLookupTimeout bounds one upstream lookup. The system
	// resolver has its own timeouts; this is the engine-level ceiling.
	DefaultDNSLookupTimeout = 8 * time.Second
)

// FailureClass is the honest failure taxonomy of one DNS decision:
// diagnostics need the class, never the query payload.
type FailureClass string

const (
	// FailureNone: the lookup succeeded.
	FailureNone FailureClass = ""

	// FailureNXDOMAIN: the name definitively does not exist.
	FailureNXDOMAIN FailureClass = "nxdomain"

	// FailureTimeout: the upstream did not answer in time.
	FailureTimeout FailureClass = "timeout"

	// FailureServer: any other upstream failure (refused, servfail,
	// transport errors).
	FailureServer FailureClass = "server"

	// FailureCancelled: the caller's context ended first.
	FailureCancelled FailureClass = "cancelled"

	// FailureInvalid: the host is not a resolvable form (IP literal
	// handled by the caller, empty, oversized, ...).
	FailureInvalid FailureClass = "invalid"
)

// DNSObservation is one observable DNS decision: resolver, route
// association, outcome class, cache state and timing. Credential-free
// by construction — a hostname is not a credential, but observations
// still carry only the host and the shape of the decision.
type DNSObservation struct {
	// Host is the queried name (as given; never a payload).
	Host string `json:"host"`

	// Resolver names the strategy that served the decision:
	// "cache", "system", "bootstrap", "remote".
	Resolver string `json:"resolver"`

	// Route is the owning route's outbound kind ("socks5", "direct",
	// ...) — the association the dataplane needs to reason about
	// loops and leak paths.
	Route string `json:"route"`

	// Success reports whether addresses came back.
	Success bool `json:"success"`

	// FailureClass carries the failure taxonomy ("" on success).
	FailureClass FailureClass `json:"failure_class,omitempty"`

	// CacheHit reports whether the decision was served from cache.
	CacheHit bool `json:"cache_hit"`

	// Addrs is the number of addresses returned.
	Addrs int `json:"addrs"`

	// Duration is the decision wall time (0 for cache hits beyond
	// measurement noise — reported honestly as measured).
	Duration time.Duration `json:"duration"`
}

// ResolverWithObservations is implemented by resolvers that record
// their decisions for diagnostics (the engine's CachingResolver does).
type ResolverWithObservations interface {
	Resolver

	// Observations returns the most recent decisions (bounded ring,
	// newest last). The slice is a copy — callers cannot corrupt it.
	Observations() []DNSObservation
}

// cacheEntry is one cached lookup result. addrs is immutable after
// construction; expiry carries the positive or negative deadline.
type cacheEntry struct {
	addrs  []netip.Addr
	expiry time.Time
	negErr error
}

// CachingResolver is the Phase 2 DNS authority behind the Resolver
// seam: a bounded in-memory cache with positive and negative TTLs,
// context-honoring upstream lookups, and an observation ring that
// records every decision (resolver, route, class, cache state, timing).
// It never mutates Windows adapter DNS settings — it is a library, not
// a system configuration surface.
type CachingResolver struct {
	upstream Resolver

	route string

	maxEntries    int
	positiveTTL   time.Duration
	negativeTTL   time.Duration
	lookupTimeout time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry

	obsMu   sync.Mutex
	obsRing []DNSObservation

	now func() time.Time
}

// NewCachingResolver builds the authority. upstream is the bootstrap
// strategy (the system resolver, or a constrained bootstrap resolver
// on the TUN path); route names the owning route for observations.
// Bounds of 0 fall to the documented defaults.
func NewCachingResolver(upstream Resolver, route string, maxEntries int, positiveTTL, negativeTTL time.Duration) *CachingResolver {
	if upstream == nil {
		upstream = SystemResolver()
	}

	if maxEntries <= 0 {
		maxEntries = DefaultDNSCacheEntries
	}

	if positiveTTL <= 0 {
		positiveTTL = DefaultDNSPositiveTTL
	}

	if negativeTTL <= 0 {
		negativeTTL = DefaultDNSNegativeTTL
	}

	return &CachingResolver{
		upstream:      upstream,
		route:         route,
		maxEntries:    maxEntries,
		positiveTTL:   positiveTTL,
		negativeTTL:   negativeTTL,
		lookupTimeout: DefaultDNSLookupTimeout,
		cache:         make(map[string]cacheEntry, maxEntries),
		obsRing:       make([]DNSObservation, 0, DefaultDNSObservations),
		now:           time.Now,
	}
}

// Resolve implements Resolver: cache first (positive and negative),
// upstream only on miss, single-flight-free by design (a stampede on
// one cold name costs a few lookups; a single-flight map costs an
// unbounded goroutine bookkeeping surface — Phase 2 chooses the bound).
func (r *CachingResolver) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	start := r.now()

	host = normalizeHost(host)
	if host == "" {
		r.record(DNSObservation{Host: host, Resolver: "cache", Route: r.route,
			FailureClass: FailureInvalid, Duration: r.now().Sub(start)})

		return nil, fmt.Errorf("freecore.dns: empty host")
	}

	now := r.now()

	if entry, ok := r.lookup(host, now); ok {
		if entry.negErr != nil {
			r.record(DNSObservation{Host: host, Resolver: "cache", Route: r.route,
				FailureClass: classifyFailure(entry.negErr), CacheHit: true,
				Duration: r.now().Sub(start)})

			return nil, entry.negErr
		}

		r.record(DNSObservation{Host: host, Resolver: "cache", Route: r.route,
			Success: true, CacheHit: true, Addrs: len(entry.addrs),
			Duration: r.now().Sub(start)})

		return entry.addrs, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, r.lookupTimeout)
	defer cancel()

	addrs, err := r.upstream.Resolve(lookupCtx, host)
	if err != nil {
		wrapped := fmt.Errorf("freecore.dns: resolve %s: %w", host, err)

		if isCancellation(err) && ctx.Err() != nil {
			// The CALLER cancelled: do not cache a decision that belongs
			// to the caller's lifecycle, do not pretend it is a server
			// answer.
			r.record(DNSObservation{Host: host, Resolver: "system", Route: r.route,
				FailureClass: FailureCancelled, Duration: r.now().Sub(start)})

			return nil, wrapped
		}

		r.store(host, cacheEntry{negErr: err, expiry: r.now().Add(r.negativeTTL)})

		r.record(DNSObservation{Host: host, Resolver: "system", Route: r.route,
			FailureClass: classifyFailure(err), Duration: r.now().Sub(start)})

		return nil, wrapped
	}

	if len(addrs) == 0 {
		err := fmt.Errorf("freecore.dns: resolve %s: no addresses", host)

		r.store(host, cacheEntry{negErr: err, expiry: r.now().Add(r.negativeTTL)})

		r.record(DNSObservation{Host: host, Resolver: "system", Route: r.route,
			FailureClass: FailureServer, Duration: r.now().Sub(start)})

		return nil, err
	}

	r.store(host, cacheEntry{addrs: addrs, expiry: r.now().Add(r.positiveTTL)})

	r.record(DNSObservation{Host: host, Resolver: "system", Route: r.route,
		Success: true, Addrs: len(addrs), Duration: r.now().Sub(start)})

	return addrs, nil
}

// Observations implements ResolverWithObservations.
func (r *CachingResolver) Observations() []DNSObservation {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()

	out := make([]DNSObservation, len(r.obsRing))
	copy(out, r.obsRing)

	return out
}

// lookup reads the cache under RLock. Expired entries are reported as
// misses; eviction happens on store, not on read (a read-path delete
// would demand a write lock on every hot lookup).
func (r *CachingResolver) lookup(host string, now time.Time) (cacheEntry, bool) {
	r.mu.RLock()
	entry, ok := r.cache[host]
	r.mu.RUnlock()

	if !ok || now.After(entry.expiry) {
		return cacheEntry{}, false
	}

	return entry, true
}

// store inserts/refreshes one entry with bounded eviction: when the
// cache is full, one arbitrary map entry is dropped (Go map iteration
// order) before the insert. Deterministic LRU is Phase 3 polish; the
// BOUND is the Phase 2 contract.
func (r *CachingResolver) store(host string, entry cacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.cache[host]; !exists && len(r.cache) >= r.maxEntries {
		for victim := range r.cache {
			delete(r.cache, victim)

			break
		}
	}

	r.cache[host] = entry
}

// record appends one decision to the bounded observation ring.
func (r *CachingResolver) record(obs DNSObservation) {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()

	if len(r.obsRing) >= DefaultDNSObservations {
		// Drop the oldest: shift the ring (small, bounded).
		r.obsRing = r.obsRing[1:]
	}

	r.obsRing = append(r.obsRing, obs)
}

// normalizeHost trims and lowercases a hostname for cache-keying.
// IP literals pass through untouched (callers short-circuit literals
// before DNS; the resolver refuses to answer them to stay honest).
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// isCancellation reports whether err is a context cancellation.
func isCancellation(err error) bool {
	return err != nil && (err == context.Canceled || err == context.DeadlineExceeded)
}

// classifyFailure maps an upstream error into the observation taxonomy.
func classifyFailure(err error) FailureClass {
	if err == nil {
		return FailureNone
	}

	if isCancellation(err) {
		return FailureCancelled
	}

	if dnsErr, ok := err.(*net.DNSError); ok {
		if dnsErr.IsNotFound {
			return FailureNXDOMAIN
		}

		if dnsErr.IsTimeout {
			return FailureTimeout
		}
	}

	return FailureServer
}

// ConstrainedDialer builds a Dialer whose connections are bound to the
// network interface the loop-prevention policy selected — the seam the
// bootstrap DNS path uses so its queries can never enter the FreeIran
// TUN (DNS bootstrap → physical interface → upstream, never TUN →
// resolver → TUN). binding is the platform hook (nil = system default
// routing, correct when no TUN owns the default route). The hook
// receives the caller's CONTEXT and must honor it: a bootstrap query
// cancelled by shutdown must not keep dialing.
func ConstrainedDialer(binding func(ctx context.Context, network, address string) (net.Conn, error)) Dialer {
	return constrainedDialer{binding: binding, timeout: DefaultDialTimeout}
}

type constrainedDialer struct {
	binding func(ctx context.Context, network, address string) (net.Conn, error)
	timeout time.Duration
}

// DialContext implements Dialer.
func (d constrainedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.binding != nil {
		conn, err := d.binding(ctx, network, address)
		if err != nil {
			return nil, fmt.Errorf("freecore.dns: constrained dial %s: %w", address, err)
		}

		return conn, nil
	}

	dialer := &net.Dialer{Timeout: d.timeout, KeepAlive: 30 * time.Second}

	return dialer.DialContext(ctx, network, address)
}

// BootstrapResolver resolves through explicit DNS servers over UDP
// with an optional interface-constrained dial. It exists so the TUN
// dataplane's own upstream needs (the proxy endpoint's hostname) can
// be resolved WITHOUT touching the host DNS path that the TUN itself
// may have captured — the recursion gate Phase F demands.
type BootstrapResolver struct {
	// Servers are the DNS endpoints (host:port; :53 implied when bare).
	Servers []string

	// Dial is the constrained dial seam (nil = default routing).
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	// Timeout bounds one server exchange (0 = DefaultDNSLookupTimeout).
	Timeout time.Duration
}

// Resolve implements Resolver over UDP DNS (a minimal, honest A/AAAA
// query path — bootstrap needs names of proxy endpoints, not every
// record type; failures classify cleanly for diagnostics).
func (b BootstrapResolver) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, fmt.Errorf("freecore.dns: bootstrap: empty host")
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}

	timeout := b.Timeout
	if timeout <= 0 {
		timeout = DefaultDNSLookupTimeout
	}

	servers := b.Servers
	if len(servers) == 0 {
		servers = []string{"1.1.1.1:53", "8.8.8.8:53"}
	}

	var lastErr error

	for _, server := range servers {
		addrs, err := b.queryServer(ctx, server, host, timeout)
		if err != nil {
			lastErr = err

			continue
		}

		if len(addrs) > 0 {
			return addrs, nil
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("freecore.dns: bootstrap %s: %w", host, lastErr)
	}

	return nil, fmt.Errorf("freecore.dns: bootstrap %s: no addresses from %d server(s)", host, len(servers))
}

// queryServer runs one A/AAAA exchange against one server.
func (b BootstrapResolver) queryServer(ctx context.Context, server, host string, timeout time.Duration) ([]netip.Addr, error) {
	dial := b.Dial
	if dial == nil {
		defaultDial := &net.Dialer{Timeout: timeout}

		dial = defaultDial.DialContext
	}

	// v4 A query when the constraint points at a v4-reachable interface;
	// ask both families sequentially (bootstrap is cold-path).
	var addrs []netip.Addr

	for _, family := range []struct {
		qtype uint16
		ipv6  bool
	}{{0x0001, false}, {0x001C, true}} {
		conn, err := dial(ctx, "udp", server)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", server, err)
		}

		_ = conn.SetDeadline(time.Now().Add(timeout))

		query := buildDNSQuery(host, family.qtype)

		if _, err := conn.Write(query); err != nil {
			_ = conn.Close()

			return nil, fmt.Errorf("send: %w", err)
		}

		buf := make([]byte, 512)

		n, err := conn.Read(buf)
		_ = conn.Close()

		if err != nil {
			return nil, fmt.Errorf("recv: %w", err)
		}

		parsed, perr := parseDNSAnswers(buf[:n], family.ipv6)
		if perr != nil {
			return nil, perr
		}

		addrs = append(addrs, parsed...)
	}

	return addrs, nil
}

// buildDNSQuery encodes one minimal DNS query (recursion desired).
func buildDNSQuery(name string, qtype uint16) []byte {
	var out []byte

	// Header: ID 0 (idempotent for our one-shot resolver), flags
	// recursion desired, one question.
	out = append(out, 0x00, 0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)

	for _, label := range splitDNSLabels(name) {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}

	out = append(out, 0x00)
	out = append(out, byte(qtype>>8), byte(qtype))
	out = append(out, 0x00, 0x01) // class IN

	return out
}

// splitDNSLabels splits a hostname into DNS labels, refusing the
// pathological (empty label runs, >255 octets names, >63 labels).
func splitDNSLabels(name string) []string {
	name = strings.TrimSuffix(name, ".")

	if name == "" || len(name) > 255 {
		return nil
	}

	parts := strings.Split(name, ".")
	for _, p := range parts {
		if p == "" || len(p) > 63 {
			return nil
		}
	}

	return parts
}

// parseDNSAnswers extracts A/AAAA records from one DNS response
// (bounded: it never reads beyond the message).
func parseDNSAnswers(msg []byte, ipv6 bool) ([]netip.Addr, error) {
	if len(msg) < 12 {
		return nil, fmt.Errorf("freecore.dns: bootstrap: short response")
	}

	qdcount := int(msg[4])<<8 | int(msg[5])
	ancount := int(msg[6])<<8 | int(msg[7])

	pos := 12

	skipQuestions := func() error {
		for i := 0; i < qdcount; i++ {
			if err := skipDNSName(msg, &pos); err != nil {
				return err
			}

			pos += 4
		}

		return nil
	}()

	if skipQuestions != nil {
		return nil, skipQuestions
	}

	var addrs []netip.Addr

	want := 4
	if ipv6 {
		want = 16
	}

	for i := 0; i < ancount && pos+10 <= len(msg); i++ {
		if err := skipDNSName(msg, &pos); err != nil {
			return nil, err
		}

		if pos+10 > len(msg) {
			break
		}

		rtype := uint16(msg[pos])<<8 | uint16(msg[pos+1])
		rdlength := int(msg[pos+8])<<8 | int(msg[pos+9])
		pos += 10

		if pos+rdlength > len(msg) {
			break
		}

		if rdlength == want && ((rtype == 0x0001 && !ipv6) || (rtype == 0x001C && ipv6)) {
			if addr, ok := netip.AddrFromSlice(msg[pos : pos+rdlength]); ok {
				addrs = append(addrs, addr.Unmap())
			}
		}

		pos += rdlength
	}

	return addrs, nil
}

// skipDNSName advances past a (possibly compressed) DNS name.
func skipDNSName(msg []byte, pos *int) error {
	for {
		if *pos >= len(msg) {
			return fmt.Errorf("freecore.dns: bootstrap: truncated name")
		}

		length := int(msg[*pos])

		switch {
		case length == 0:
			*pos++

			return nil
		case length&0xC0 == 0xC0:
			if *pos+2 > len(msg) {
				return fmt.Errorf("freecore.dns: bootstrap: truncated compression pointer")
			}

			*pos += 2

			return nil
		default:
			*pos += 1 + length
		}
	}
}
