// dnsdiag.go implements the v0.9.8.5 DNS Diagnostic (§5): the
// existing DNS tool (a single hostname resolution) expanded into a
// proper diagnostic that compares the system resolver against a
// small, bounded set of curated public resolvers, per record type,
// with explicit transports and honest failure classification.
//
//	System       A     24 ms   2 answers   OK
//	             AAAA  27 ms   2 answers   OK
//	Cloudflare   A     18 ms   2 answers   OK
//	             AAAA  20 ms   2 answers   OK
//	...
//
// Design rules (§5.6 DNS safety):
//
//   - bounded: a small curated resolver set, one bounded query per
//     (resolver, record type), per-query timeout, overall deadline;
//   - cancellable at every step;
//   - query names validated before any bytes leave the machine;
//   - user-supplied resolvers must be public IP literals (private /
//     loopback targets are rejected — the private-target policy);
//   - response sizes are bounded; malformed responses are classified,
//     never trusted;
//   - no DNSSEC claim is made — this is a plain diagnostic query
//     engine (no AD-bit interpretation, no validation);
//   - runs ONLY on explicit user action (the service layer enforces
//     it — no automatic background queries).
package netcheck

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DNSRecordType selects the query type of one diagnostic row.
type DNSRecordType string

// The supported record types (§5.2: A + AAAA; additional diagnostic
// records are deliberately NOT added — no unsafe complexity).
const (
	DNSTypeA    DNSRecordType = "A"
	DNSTypeAAAA DNSRecordType = "AAAA"
)

// qtypeCode maps a record type to its wire QTYPE value.
func qtypeCode(t DNSRecordType) (uint16, bool) {
	switch t {
	case DNSTypeA:
		return 1, true
	case DNSTypeAAAA:
		return 28, true
	default:
		return 0, false
	}
}

// CuratedResolver is one curated public resolver (§5.1: few,
// deterministic, independently operated).
type CuratedResolver struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// CuratedDNSResolvers is the bounded comparison set.
var CuratedDNSResolvers = []CuratedResolver{
	{Name: "Cloudflare", Address: "1.1.1.1"},
	{Name: "Google", Address: "8.8.8.8"},
	{Name: "Quad9", Address: "9.9.9.9"},
}

// DefaultDNSProbeName is the default query name of a diagnostic run.
const DefaultDNSProbeName = "www.gstatic.com"

// MaxDNSDiagnosticResolvers bounds the resolver rows of one run
// (system + curated set + a small user-supplied allowance).
const MaxDNSDiagnosticResolvers = 6

// DNSFailureClass is the honest classification of one DNS failure
// (§5.5) — never a single generic "internet failure".
type DNSFailureClass string

const (
	DNSFailNone        DNSFailureClass = ""
	DNSFailTimeout     DNSFailureClass = "timeout"
	DNSFailRefused     DNSFailureClass = "refused"
	DNSFailServfail    DNSFailureClass = "servfail"
	DNSFailNXDomain    DNSFailureClass = "nxdomain"
	DNSFailEmpty       DNSFailureClass = "empty_answer"
	DNSFailMalformed   DNSFailureClass = "malformed"
	DNSFailUnreachable DNSFailureClass = "resolver_unreachable"
	DNSFailCancelled   DNSFailureClass = "cancelled"
	DNSFailInvalid     DNSFailureClass = "invalid_target"
)

// HumanLabel renders a short failure label for the UI.
func (c DNSFailureClass) HumanLabel() string {
	switch c {
	case DNSFailNone:
		return "OK"
	case DNSFailTimeout:
		return "timeout"
	case DNSFailRefused:
		return "refused"
	case DNSFailServfail:
		return "server failure"
	case DNSFailNXDomain:
		return "no such domain"
	case DNSFailEmpty:
		return "no answers"
	case DNSFailMalformed:
		return "malformed response"
	case DNSFailUnreachable:
		return "resolver unreachable"
	case DNSFailCancelled:
		return "cancelled"
	case DNSFailInvalid:
		return "invalid target"
	default:
		return string(c)
	}
}

// DNSQueryResult is one (resolver, record type) row of evidence.
type DNSQueryResult struct {
	Resolver     string          `json:"resolver"`
	Address      string          `json:"address,omitempty"` // resolver address ("" = system)
	Transport    string          `json:"transport"`         // system | udp | tcp
	QueryName    string          `json:"query_name"`
	RecordType   DNSRecordType   `json:"record_type"`
	OK           bool            `json:"ok"`
	Status       int             `json:"status,omitempty"` // DNS RCODE
	LatencyMS    int64           `json:"latency_ms,omitempty"`
	Measured     bool            `json:"measured,omitempty"`
	AnswerCount  int             `json:"answer_count,omitempty"`
	Addresses    []string        `json:"addresses,omitempty"` // bounded
	FailureClass DNSFailureClass `json:"failure_class,omitempty"`
	Error        string          `json:"error,omitempty"`
	At           time.Time       `json:"at"`
}

// DNSResolverResult is one resolver's full row (A + AAAA).
type DNSResolverResult struct {
	Resolver  string           `json:"resolver"`
	Address   string           `json:"address,omitempty"`
	Transport string           `json:"transport,omitempty"` // dominant transport
	OK        bool             `json:"ok"`                  // all queries OK
	Queries   []DNSQueryResult `json:"queries"`
	LatencyMS int64            `json:"latency_ms,omitempty"` // best successful
}

// DNSDiagnosticReport is the outcome of ONE user-triggered run.
type DNSDiagnosticReport struct {
	Name       string              `json:"name"`
	Resolvers  []DNSResolverResult `json:"resolvers"`
	StartedAt  time.Time           `json:"started_at"`
	DurationMS int64               `json:"duration_ms"`
	Cancelled  bool                `json:"cancelled,omitempty"`
}

// DNSDiagnosticOptions tunes one run. The zero value selects the
// default probe name, the system resolver plus the curated set, and
// A + AAAA.
type DNSDiagnosticOptions struct {
	// Name is the query name (default DefaultDNSProbeName).
	Name string

	// Resolvers optionally narrows the comparison to explicit public
	// resolver IP literals (bounded to MaxDNSDiagnosticResolvers).
	// Empty: system + curated set.
	Resolvers []string

	// Types selects the record types (default A + AAAA).
	Types []DNSRecordType

	// Timeout bounds ONE query round trip (0 = 3s, hard cap 10s).
	Timeout time.Duration

	// Dial routes the resolver transport through the ACTIVE tunnel
	// when set (nil = direct). System-resolver rows are never run on a
	// tunneled diagnostic — they would silently bypass the tunnel.
	Dial DialFunc

	// AllowPrivateResolvers permits private resolver addresses. The
	// tool surface NEVER sets it (the private-target policy applies —
	// public resolvers only); it exists for deterministic local
	// test harnesses.
	AllowPrivateResolvers bool
}

// dnsQueryTimeout is the per-query default.
const dnsQueryTimeout = 3 * time.Second

func (o DNSDiagnosticOptions) normalize() (DNSDiagnosticOptions, error) {
	if o.Name == "" {
		o.Name = DefaultDNSProbeName
	}

	if err := ValidateDNSQueryName(o.Name); err != nil {
		return o, err
	}

	if o.Timeout <= 0 {
		o.Timeout = dnsQueryTimeout
	}

	if o.Timeout > 10*time.Second {
		o.Timeout = 10 * time.Second
	}

	if len(o.Types) == 0 {
		o.Types = []DNSRecordType{DNSTypeA, DNSTypeAAAA}
	}

	types := make([]DNSRecordType, 0, len(o.Types))
	for _, t := range o.Types {
		if _, ok := qtypeCode(t); ok {
			types = append(types, t)
		}
	}

	o.Types = types

	return o, nil
}

// ValidateDNSQueryName accepts a syntactically valid public DNS name
// and rejects everything else BEFORE any bytes leave the machine
// (IP literals, empty labels, over-long names, invalid characters).
func ValidateDNSQueryName(name string) error {
	name = strings.TrimSpace(name)

	if name == "" {
		return errors.New("empty query name")
	}

	if ip := net.ParseIP(name); ip != nil {
		return errors.New("query name must be a hostname, not an IP address")
	}

	if len(name) > 253 {
		return errors.New("query name longer than 253 characters")
	}

	if strings.Contains(name, "://") {
		return errors.New("query name must be a bare hostname, not a URL")
	}

	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return errors.New("empty label in query name")
		}

		if len(label) > 63 {
			return errors.New("label longer than 63 characters")
		}

		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '-' || r == '_':
			default:
				return fmt.Errorf("invalid character %q in query name", r)
			}
		}
	}

	return nil
}

// RunDNSDiagnostic executes one bounded, cancellable comparison run.
// Resolvers are probed concurrently (bounded by the set size), the
// record types sequentially within a resolver; a truncation (TC bit)
// or a UDP transport failure falls back to TCP once — the transport
// is recorded per row.
func RunDNSDiagnostic(ctx context.Context, opts DNSDiagnosticOptions) DNSDiagnosticReport {
	report := DNSDiagnosticReport{StartedAt: time.Now().UTC()}

	opts, err := opts.normalize()
	if err != nil {
		// Surface the invalid-target rejection on every row — the
		// report shape stays stable for the UI.
		report.Resolvers = []DNSResolverResult{{
			Resolver: "invalid",
			OK:       false,
			Queries: []DNSQueryResult{{
				QueryName: opts.Name, Transport: "none",
				FailureClass: DNSFailInvalid, Error: err.Error(),
				At: time.Now().UTC(),
			}},
		}}

		report.DurationMS = 1

		return report
	}

	report.Name = opts.Name

	specs := []struct {
		label   string
		address string
		system  bool
	}{}

	if len(opts.Resolvers) > 0 {
		// Explicit resolver set: exactly the requested rows (bounded).
		for _, addr := range opts.Resolvers {
			if len(specs) >= MaxDNSDiagnosticResolvers {
				break
			}

			specs = append(specs, struct {
				label   string
				address string
				system  bool
			}{label: "Resolver " + addr, address: addr})
		}
	} else {
		// Default comparison: system + curated (§5.1).
		specs = append(specs, struct {
			label   string
			address string
			system  bool
		}{label: "System", address: "", system: true})

		for _, curated := range CuratedDNSResolvers {
			specs = append(specs, struct {
				label   string
				address string
				system  bool
			}{label: curated.Name, address: curated.Address})
		}
	}

	results := make([]DNSResolverResult, len(specs))

	var wg sync.WaitGroup

	for i, spec := range specs {
		wg.Add(1)

		go func(slot int, label, address string, system bool) {
			defer wg.Done()
			results[slot] = runOneResolver(ctx, label, address, system, opts)
		}(i, spec.label, spec.address, spec.system)
	}

	wg.Wait()

	report.Resolvers = results
	report.DurationMS = maxI64(time.Since(report.StartedAt).Milliseconds(), 1)
	report.Cancelled = ctx.Err() != nil

	return report
}

// resolverSpecFor renders the resolver label of an address.
func resolverSpecFor(opts DNSDiagnosticOptions, address string) string {
	for _, curated := range CuratedDNSResolvers {
		if curated.Address == address {
			return curated.Name
		}
	}

	return "Resolver " + address
}

// runOneResolver probes A + AAAA through one resolver sequentially.
func runOneResolver(
	ctx context.Context,
	label, address string,
	system bool,
	opts DNSDiagnosticOptions,
) DNSResolverResult {
	result := DNSResolverResult{Resolver: label, Address: address}

	// Private-resolver rejection (§5.6): user-supplied resolvers must
	// be public IP literals (an optional :port is allowed for
	// non-standard local test harnesses and split-horizon ports).
	if !system && !opts.AllowPrivateResolvers {
		host, _, splitErr := splitResolverAddress(address)
		if splitErr != nil {
			result.Queries = invalidQueries(opts, splitErr.Error())

			return result
		}

		ip := net.ParseIP(host)
		if ip == nil {
			result.Queries = invalidQueries(opts, fmt.Sprintf("resolver %q is not an IP literal", host))

			return result
		}

		if IsPrivateIP(ip) {
			result.Queries = invalidQueries(opts,
				"private resolver blocked by policy (public resolvers only)")

			return result
		}
	}

	for _, recordType := range opts.Types {
		if ctx.Err() != nil {
			result.Queries = append(result.Queries, cancelledQuery(label, address, opts.Name, recordType))

			continue
		}

		query := runOneQuery(ctx, label, address, system, opts.Name, recordType, opts.Timeout, opts.Dial)
		result.Queries = append(result.Queries, query)

		if query.OK && (result.LatencyMS == 0 || query.LatencyMS < result.LatencyMS) {
			result.LatencyMS = query.LatencyMS
		}
	}

	result.OK = allQueriesOK(result.Queries)
	result.Transport = dominantTransport(result.Queries)

	return result
}

// invalidQueries renders a per-type invalid rejection row set.
func invalidQueries(opts DNSDiagnosticOptions, reason string) []DNSQueryResult {
	queries := make([]DNSQueryResult, 0, len(opts.Types))

	for _, t := range opts.Types {
		queries = append(queries, DNSQueryResult{
			QueryName: opts.Name, RecordType: t, Transport: "none",
			FailureClass: DNSFailInvalid, Error: reason, At: time.Now().UTC(),
		})
	}

	return queries
}

func cancelledQuery(label, address, name string, t DNSRecordType) DNSQueryResult {
	return DNSQueryResult{
		Resolver: label, Address: address, QueryName: name, RecordType: t,
		Transport: "none", FailureClass: DNSFailCancelled, Error: "cancelled",
		At: time.Now().UTC(),
	}
}

func allQueriesOK(queries []DNSQueryResult) bool {
	if len(queries) == 0 {
		return false
	}

	for _, q := range queries {
		if !q.OK {
			return false
		}
	}

	return true
}

func dominantTransport(queries []DNSQueryResult) string {
	counts := map[string]int{}

	for _, q := range queries {
		if q.Transport != "" && q.Transport != "none" {
			counts[q.Transport]++
		}
	}

	best, bestN := "", 0
	for transport, n := range counts {
		if n > bestN {
			best, bestN = transport, n
		}
	}

	return best
}

// runOneQuery executes ONE (resolver, type) lookup with UDP→TCP
// fallback for the explicit-resolver path, or the system resolver API
// otherwise. The wire transport honours the tunnel dialer when one
// is supplied (a tunneled diagnostic never leaks around the tunnel).
func runOneQuery(
	ctx context.Context,
	label, address string,
	system bool,
	name string,
	recordType DNSRecordType,
	timeout time.Duration,
	dial DialFunc,
) DNSQueryResult {
	query := DNSQueryResult{
		Resolver: label, Address: address,
		QueryName: name, RecordType: recordType,
		At: time.Now().UTC(),
	}

	if system {
		query.Transport = "system"

		resolver := &net.Resolver{}

		network := "ip4"
		if recordType == DNSTypeAAAA {
			network = "ip6"
		}

		started := time.Now()

		addrs, err := resolver.LookupIP(ctx, network, name)
		elapsed := time.Since(started)

		query.LatencyMS = maxI64(elapsed.Milliseconds(), 0)
		query.Measured = true

		if err != nil {
			query.FailureClass, query.Error = classifySystemDNSError(err)
			query.LatencyMS = 0 // failed lookups carry no latency claim

			return query
		}

		if len(addrs) == 0 {
			query.FailureClass = DNSFailEmpty
			query.Error = "resolver returned no addresses"

			return query
		}

		return finishSuccessfulQuery(&query, addrs)
	}

	// Explicit resolver: raw wire protocol, UDP first, TCP fallback.
	qtype, _ := qtypeCode(recordType)

	wire, err := buildDNSWireQuery(name, qtype)
	if err != nil {
		query.FailureClass = DNSFailInvalid
		query.Error = err.Error()

		return query
	}

	host, port, splitErr := splitResolverAddress(address)
	if splitErr != nil {
		query.FailureClass = DNSFailInvalid
		query.Error = splitErr.Error()

		return query
	}

	server := net.JoinHostPort(host, port)

	started := time.Now()

	response, transport, err := dnsExchange(ctx, server, wire, timeout, dial)
	elapsed := time.Since(started)

	query.Transport = transport
	query.LatencyMS = elapsed.Milliseconds()
	query.Measured = true

	if err != nil {
		query.LatencyMS = 0
		query.FailureClass, query.Error = classifyExchangeError(err)

		return query
	}

	parsed, err := parseDNSResponse(response)
	if err != nil {
		query.FailureClass = DNSFailMalformed
		query.Error = err.Error()

		return query
	}

	query.Status = parsed.rcode

	switch parsed.rcode {
	case 0: // NOERROR
	case 2:
		query.FailureClass = DNSFailServfail
		query.Error = "resolver returned SERVFAIL"

		return query
	case 3:
		query.FailureClass = DNSFailNXDomain
		query.Error = "domain does not exist"

		return query
	case 5:
		query.FailureClass = DNSFailRefused
		query.Error = "resolver refused the query"

		return query
	default:
		query.FailureClass = DNSFailMalformed
		query.Error = fmt.Sprintf("resolver returned RCODE %d", parsed.rcode)

		return query
	}

	answers := addressesOf(parsed, recordType)

	if len(parsed.answers) == 0 || len(answers) == 0 {
		query.FailureClass = DNSFailEmpty
		query.Error = "no answers of this type"

		return query
	}

	return finishSuccessfulQuery(&query, answers)
}

// finishSuccessfulQuery stamps the successful evidence (bounded
// address list).
func finishSuccessfulQuery(query *DNSQueryResult, addrs []net.IP) DNSQueryResult {
	query.OK = true
	query.AnswerCount = len(addrs)
	query.Addresses = boundedIPs(addrs, 8)

	return *query
}

// classifySystemDNSError maps a stdlib resolver error onto the
// diagnostic failure classes.
func classifySystemDNSError(err error) (DNSFailureClass, string) {
	if err == nil {
		return DNSFailNone, ""
	}

	if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "operation was canceled") {
		return DNSFailCancelled, "cancelled"
	}

	var dnsErr *net.DNSError

	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return DNSFailNXDomain, "domain does not exist"
		case dnsErr.IsTimeout:
			return DNSFailTimeout, "resolver timed out"
		case dnsErr.IsTemporary:
			return DNSFailTimeout, "temporary resolver failure"
		default:
			return DNSFailUnreachable, dnsErr.Err
		}
	}

	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return DNSFailTimeout, err.Error()
	case strings.Contains(msg, "refused"):
		return DNSFailRefused, err.Error()
	case strings.Contains(msg, "no such host"):
		return DNSFailNXDomain, err.Error()
	case strings.Contains(msg, "malformed"):
		return DNSFailMalformed, err.Error()
	default:
		return DNSFailUnreachable, err.Error()
	}
}

// classifyExchangeError maps a wire-transport error onto classes.
func classifyExchangeError(err error) (DNSFailureClass, string) {
	if err == nil {
		return DNSFailNone, ""
	}

	if errors.Is(err, context.Canceled) {
		return DNSFailCancelled, "cancelled"
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return DNSFailTimeout, "resolver timed out"
	}

	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"), strings.Contains(msg, "i/o timeout"):
		return DNSFailTimeout, "resolver timed out"
	case strings.Contains(msg, "refused"):
		return DNSFailRefused, "resolver refused the connection"
	case strings.Contains(msg, "unreachable"), strings.Contains(msg, "no route"):
		return DNSFailUnreachable, "resolver unreachable"
	default:
		return DNSFailUnreachable, err.Error()
	}
}

// splitResolverAddress splits a resolver address of the form
// "host", "host:port" or "[v6]:port" into its host and port parts
// (default port 53, validated range).
func splitResolverAddress(address string) (string, string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", "", errors.New("empty resolver address")
	}

	if host, port, err := net.SplitHostPort(address); err == nil {
		if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
			return "", "", fmt.Errorf("invalid resolver port %q", port)
		}

		return host, port, nil
	}

	return address, "53", nil
}

// boundedIPs renders bounded, deduplicated IP strings.
func boundedIPs(addrs []net.IP, limit int) []string {
	seen := make(map[string]struct{}, len(addrs))
	out := make([]string, 0, limit)

	for _, ip := range addrs {
		s := ip.String()
		if _, dup := seen[s]; dup {
			continue
		}

		seen[s] = struct{}{}

		if len(out) < limit {
			out = append(out, s)
		}
	}

	return out
}

// ---- minimal RFC 1035 wire client --------------------------------------
//
// Deliberately stdlib-only (no new dependencies): the client speaks
// just enough DNS to ask one A/AAAA question and read the matching
// answers, with bounded buffers everywhere.

// dnsMaxResponse bounds one DNS response read (RFC 1035 UDP safety).
const dnsMaxResponse = 4096

type dnsParsedResponse struct {
	id        uint16
	rcode     int
	truncated bool
	answers   []dnsAnswer
}

type dnsAnswer struct {
	qtype uint16
	addr  net.IP
}

// buildDNSWireQuery encodes one question (name, QTYPE, QCLASS IN)
// with a fresh transaction id.
func buildDNSWireQuery(name string, qtype uint16) ([]byte, error) {
	if err := ValidateDNSQueryName(name); err != nil {
		return nil, err
	}

	var out []byte

	id := uint16(rand.Int31n(0x10000)) //nolint:gosec // transaction id, not a secret

	out = append(out,
		byte(id>>8), byte(id), // ID
		0x01, 0x00, // RD=1
		0x00, 0x01, // QDCOUNT
		0x00, 0x00, // ANCOUNT
		0x00, 0x00, // NSCOUNT
		0x00, 0x00, // ARCOUNT
	)

	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 {
			return nil, fmt.Errorf("label %q longer than 63 characters", label)
		}

		out = append(out, byte(len(label)))
		out = append(out, label...)
	}

	out = append(out, 0x00) // root label

	out = append(out, byte(qtype>>8), byte(qtype)) // QTYPE
	out = append(out, 0x00, 0x01)                  // QCLASS IN

	return out, nil
}

// dnsExchange sends the query over UDP first; on a truncation (TC bit)
// or a UDP transport error it retries ONCE over TCP (§5.4). It
// returns the response bytes and the transport that produced them.
func dnsExchange(ctx context.Context, server string, query []byte, timeout time.Duration, dial DialFunc) ([]byte, string, error) {
	response, err := dnsExchangeOne(ctx, server, "udp", query, timeout, dial)
	if err == nil {
		// Truncated? fall back to TCP for the full answer.
		if len(response) >= 12 && response[2]&0x02 != 0 {
			if tcpResp, tcpErr := dnsExchangeOne(ctx, server, "tcp", query, timeout, dial); tcpErr == nil {
				return tcpResp, "tcp", nil
			}
		}

		return response, "udp", nil
	}

	// UDP failed to transport at all: one bounded TCP attempt.
	if tcpResp, tcpErr := dnsExchangeOne(ctx, server, "tcp", query, timeout, dial); tcpErr == nil {
		return tcpResp, "tcp", nil
	}

	return nil, "", err
}

// dnsExchangeOne performs one bounded wire exchange (direct or
// through the supplied tunnel dialer).
func dnsExchangeOne(ctx context.Context, server, network string, query []byte, timeout time.Duration, dial DialFunc) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		conn net.Conn
		err  error
	)

	if dial != nil {
		conn, err = dial(probeCtx, network, server)
	} else {
		dialer := &net.Dialer{Timeout: timeout}
		conn, err = dialer.DialContext(probeCtx, network, server)
	}

	if err != nil {
		return nil, err
	}

	defer conn.Close() //nolint:errcheck // best-effort close on bounded socket

	if deadline, ok := probeCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	if network == "tcp" {
		// DNS over TCP frames the message with a 2-byte length prefix.
		if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}

		if _, err := conn.Write([]byte{byte(len(query) >> 8), byte(len(query))}); err != nil {
			return nil, err
		}
	}

	if _, err := conn.Write(query); err != nil {
		return nil, err
	}

	buf := make([]byte, dnsMaxResponse)

	if network == "tcp" {
		// Read the length prefix first.
		var length [2]byte

		if _, err := readFull(conn, length[:]); err != nil {
			return nil, err
		}

		size := int(length[0])<<8 | int(length[1])
		if size <= 0 || size > dnsMaxResponse {
			return nil, fmt.Errorf("unreasonable TCP DNS message length %d", size)
		}

		n, err := readFull(conn, buf[:size])
		if err != nil {
			return nil, err
		}

		return buf[:n], nil
	}

	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}

	return buf[:n], nil
}

// readFull reads exactly len(out) bytes (bounded by the deadline).
func readFull(conn net.Conn, out []byte) (int, error) {
	total := 0

	for total < len(out) {
		n, err := conn.Read(out[total:])
		if n > 0 {
			total += n
		}

		if err != nil {
			return total, err
		}
	}

	return total, nil
}

// parseDNSResponse decodes the header and the answer records of one
// DNS message (compression-aware name skipping, bounded loops).
func parseDNSResponse(resp []byte) (dnsParsedResponse, error) {
	if len(resp) < 12 {
		return dnsParsedResponse{}, fmt.Errorf("short DNS response (%d bytes)", len(resp))
	}

	parsed := dnsParsedResponse{
		id:        binary.BigEndian.Uint16(resp[0:2]),
		rcode:     int(resp[3] & 0x0f),
		truncated: resp[2]&0x02 != 0,
	}

	qdcount := int(binary.BigEndian.Uint16(resp[4:6]))
	ancount := int(binary.BigEndian.Uint16(resp[6:8]))

	if qdcount > 4 || ancount > 64 {
		return dnsParsedResponse{}, fmt.Errorf("unreasonable DNS section counts (qd=%d an=%d)", qdcount, ancount)
	}

	offset := 12

	// Skip the question section.
	for i := 0; i < qdcount; i++ {
		next, err := skipDNSName(resp, offset)
		if err != nil {
			return dnsParsedResponse{}, err
		}

		if next+4 > len(resp) {
			return dnsParsedResponse{}, fmt.Errorf("truncated question section")
		}

		offset = next + 4 // QTYPE + QCLASS
	}

	// Read the answers.
	for i := 0; i < ancount; i++ {
		next, err := skipDNSName(resp, offset)
		if err != nil {
			return dnsParsedResponse{}, err
		}

		if next+10 > len(resp) {
			return dnsParsedResponse{}, fmt.Errorf("truncated answer record")
		}

		qtype := binary.BigEndian.Uint16(resp[next : next+2])
		rdlength := int(binary.BigEndian.Uint16(resp[next+8 : next+10]))
		rdataStart := next + 10

		if rdataStart+rdlength > len(resp) {
			return dnsParsedResponse{}, fmt.Errorf("answer rdata overruns the message")
		}

		switch qtype {
		case 1: // A
			if rdlength == 4 {
				addr := make(net.IP, 4)
				copy(addr, resp[rdataStart:rdataStart+4])
				parsed.answers = append(parsed.answers, dnsAnswer{qtype: qtype, addr: addr})
			}
		case 28: // AAAA
			if rdlength == 16 {
				addr := make(net.IP, 16)
				copy(addr, resp[rdataStart:rdataStart+16])
				parsed.answers = append(parsed.answers, dnsAnswer{qtype: qtype, addr: addr})
			}
		}

		offset = rdataStart + rdlength
	}

	return parsed, nil
}

// skipDNSName advances past one (possibly compressed) domain name and
// returns the offset just after it. Compression pointers are followed
// with a bounded hop count (malicious loops must not hang the
// diagnostic).
func skipDNSName(msg []byte, offset int) (int, error) {
	jumps := 0

	for {
		if offset >= len(msg) {
			return 0, fmt.Errorf("name runs past the message end")
		}

		length := int(msg[offset])

		if length == 0 {
			return offset + 1, nil
		}

		if length&0xc0 == 0xc0 {
			// Compression pointer: this name ends here.
			if offset+2 > len(msg) {
				return 0, fmt.Errorf("truncated compression pointer")
			}

			jumps++
			if jumps > 32 {
				return 0, fmt.Errorf("compression pointer loop")
			}

			pointer := int(binary.BigEndian.Uint16(msg[offset:offset+2])) & 0x3fff
			if pointer >= len(msg) {
				return 0, fmt.Errorf("compression pointer out of bounds")
			}

			offset = pointer

			continue
		}

		if length > 63 {
			return 0, fmt.Errorf("invalid label length %d", length)
		}

		offset += 1 + length
	}
}

// addressesOf extracts the answers matching one record type.
func addressesOf(parsed dnsParsedResponse, recordType DNSRecordType) []net.IP {
	want, _ := qtypeCode(recordType)

	out := make([]net.IP, 0, len(parsed.answers))

	for _, a := range parsed.answers {
		if a.qtype == want && a.addr != nil {
			out = append(out, a.addr)
		}
	}

	return out
}

// rcodeLabel renders the RCODE meaning for reporting.
func rcodeLabel(rcode int) string {
	switch rcode {
	case 0:
		return "NOERROR"
	case 2:
		return "SERVFAIL"
	case 3:
		return "NXDOMAIN"
	case 5:
		return "REFUSED"
	default:
		return "RCODE " + strconv.Itoa(rcode)
	}
}
