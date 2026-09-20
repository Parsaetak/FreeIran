package netcheck

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// dnsdiag_test.go exercises the v0.9.8.5 DNS Diagnostic against a
// DETERMINISTIC local fake resolver (real UDP/TCP sockets, RFC 1035
// wire format) — no external network dependency:
//
//   - explicit-resolver A/AAAA answers with measured latency + transport;
//   - RCODE classification (NXDOMAIN, REFUSED, SERVFAIL);
//   - empty-answer classification;
//   - truncation (TC bit) → TCP fallback;
//   - timeout classification (silent black hole);
//   - malformed response classification;
//   - cancellation;
//   - private-resolver rejection (the tool-surface policy);
//   - query-name validation.

// fakeDNSServer is a minimal deterministic DNS responder.
type fakeDNSServer struct {
	t *testing.T

	udp *net.UDPConn
	tcp net.Listener

	// handler renders the response bytes for one parsed question
	// (nil response = stay silent). transport is "udp" or "tcp".
	handler func(transport string, query []byte) []byte
}

// startFakeDNS launches the fake resolver on a loopback port (the UDP
// and TCP transports share one port, like a real resolver).
func startFakeDNS(t *testing.T, handler func(transport string, query []byte) []byte) *fakeDNSServer {
	t.Helper()

	// TCP first: its ephemeral port is then reserved for UDP too.
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	_, portStr, err := net.SplitHostPort(tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}

	udpAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort("127.0.0.1", portStr))
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}

	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}

	srv := &fakeDNSServer{t: t, handler: handler, udp: udp, tcp: tcp}

	t.Cleanup(func() {
		_ = udp.Close()
		_ = tcp.Close()
	})

	go srv.serveUDP()
	go srv.serveTCP()

	return srv
}

func (s *fakeDNSServer) addr() string {
	return s.udp.LocalAddr().String()
}

func (s *fakeDNSServer) port() string {
	_, port, _ := net.SplitHostPort(s.addr())
	return port
}

func (s *fakeDNSServer) serveUDP() {
	buf := make([]byte, dnsMaxResponse)

	for {
		n, from, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}

		response := s.handler("udp", buf[:n])
		if response == nil {
			continue
		}

		_, _ = s.udp.WriteToUDP(response, from)
	}
}

func (s *fakeDNSServer) serveTCP() {
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			return
		}

		go func(conn net.Conn) {
			defer conn.Close() //nolint:errcheck // bounded test socket

			var length [2]byte

			if _, err := readFull(conn, length[:]); err != nil {
				return
			}

			size := int(length[0])<<8 | int(length[1])
			if size <= 0 || size > dnsMaxResponse {
				return
			}

			query := make([]byte, size)
			if _, err := readFull(conn, query); err != nil {
				return
			}

			response := s.handler("tcp", query)
			if response == nil {
				return
			}

			_, _ = conn.Write([]byte{byte(len(response) >> 8), byte(len(response))})
			_, _ = conn.Write(response)
		}(conn)
	}
}

// dnsAnswerIP renders one A/AAAA record.
func dnsAnswerIP(name string, qtype uint16, ip net.IP) []byte {
	var out []byte

	for _, label := range strings.Split(name, ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}

	out = append(out, 0x00) // root

	rdata := ip.To4()
	if qtype == 28 {
		rdata = ip.To16()
	}

	out = append(out, byte(qtype>>8), byte(qtype))
	out = append(out, 0x00, 0x01)             // class IN
	out = append(out, 0x00, 0x00, 0x00, 0x3c) // TTL 60
	out = append(out, byte(len(rdata)>>8), byte(len(rdata)))
	out = append(out, rdata...)

	return out
}

// fakeDNSResponse renders a complete DNS response message.
func fakeDNSResponse(query []byte, rcode int, truncated bool, answers [][]byte) []byte {
	if len(query) < 12 {
		return nil
	}

	flags := uint16(0x8000) // QR=1
	if truncated {
		flags |= 0x0200 // TC
	}

	// Copy the question section verbatim.
	questionEnd := 12
	if len(query) >= 12 {
		offset := 12

		for offset < len(query) {
			length := int(query[offset])
			if length == 0 {
				offset++

				break
			}

			if length&0xc0 == 0xc0 {
				offset += 2

				break
			}

			offset += 1 + length
		}

		questionEnd = offset + 4
	}

	out := make([]byte, 0, questionEnd+512)
	out = append(out, query[0], query[1])          // ID echo
	out = append(out, byte(flags>>8), byte(flags)) // flags
	out = append(out, 0x00, 0x01)                  // QDCOUNT echo
	out = append(out, byte(len(answers)>>8), byte(len(answers)))
	out = append(out, 0x00, 0x00, 0x00, 0x00) // NS/AR

	out = append(out, query[12:questionEnd]...)

	for _, answer := range answers {
		out = append(out, answer...)
	}

	// Fix the RCODE inside the flags word (byte 3 low nibble).
	out[3] = (out[3] & 0xf0) | byte(rcode&0x0f)

	return out
}

// TestDNSDiagnosticExplicitResolver verifies the wire path against the
// local fake resolver: A and AAAA rows with answers, latency and the
// udp transport.
func TestDNSDiagnosticExplicitResolver(t *testing.T) {
	var sawQtypes []uint16

	srv := startFakeDNS(t, func(_ string, query []byte) []byte {
		if len(query) < 14 {
			return nil
		}

		// QTYPE sits after the question name.
		qtype := binary.BigEndian.Uint16(query[len(query)-4 : len(query)-2])
		sawQtypes = append(sawQtypes, qtype)

		if qtype == 1 {
			return fakeDNSResponse(query, 0, false, [][]byte{
				dnsAnswerIP("example.com", 1, net.ParseIP("203.0.113.10")),
				dnsAnswerIP("example.com", 1, net.ParseIP("203.0.113.11")),
			})
		}

		if qtype == 28 {
			return fakeDNSResponse(query, 0, false, [][]byte{
				dnsAnswerIP("example.com", 28, net.ParseIP("2001:db8::10")),
			})
		}

		return fakeDNSResponse(query, 0, false, nil)
	})

	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers:             []string{srv.addr()},
		Timeout:               2 * time.Second,
		AllowPrivateResolvers: true,
	})

	if len(report.Resolvers) != 1 {
		t.Fatalf("resolver rows = %d, want 1", len(report.Resolvers))
	}

	row := report.Resolvers[0]
	if !row.OK {
		t.Fatalf("resolver row failed: %+v", row)
	}

	if len(row.Queries) != 2 {
		t.Fatalf("queries = %d, want 2 (A + AAAA)", len(row.Queries))
	}

	aFound, aaaaFound := false, false

	for _, q := range row.Queries {
		if !q.OK {
			t.Fatalf("query %s failed: %+v", q.RecordType, q)
		}

		if q.Transport != "udp" {
			t.Fatalf("transport = %s, want udp", q.Transport)
		}

		if !q.Measured {
			t.Fatalf("query %s not measured", q.RecordType)
		}

		switch q.RecordType {
		case DNSTypeA:
			aFound = true

			if q.AnswerCount != 2 || len(q.Addresses) != 2 {
				t.Fatalf("A answers = %d/%d", q.AnswerCount, len(q.Addresses))
			}
		case DNSTypeAAAA:
			aaaaFound = true

			if q.AnswerCount != 1 || q.Addresses[0] != "2001:db8::10" {
				t.Fatalf("AAAA answers = %+v", q.Addresses)
			}
		}
	}

	if !aFound || !aaaaFound {
		t.Fatalf("missing record types: A=%v AAAA=%v", aFound, aaaaFound)
	}
}

// TestDNSDiagnosticNXDomain verifies the NXDOMAIN classification.
func TestDNSDiagnosticNXDomain(t *testing.T) {
	srv := startFakeDNS(t, func(_ string, query []byte) []byte {
		return fakeDNSResponse(query, 3, false, nil) // NXDOMAIN
	})

	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers:             []string{srv.addr()},
		Timeout:               2 * time.Second,
		AllowPrivateResolvers: true,
	})

	for _, q := range report.Resolvers[0].Queries {
		if q.FailureClass != DNSFailNXDomain {
			t.Fatalf("failure class = %s, want nxdomain (%s)", q.FailureClass, q.Error)
		}
	}

	if report.Resolvers[0].OK {
		t.Fatal("NXDOMAIN must not count as OK")
	}
}

// TestDNSDiagnosticRefused verifies the REFUSED classification.
func TestDNSDiagnosticRefused(t *testing.T) {
	srv := startFakeDNS(t, func(_ string, query []byte) []byte {
		return fakeDNSResponse(query, 5, false, nil) // REFUSED
	})

	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers:             []string{srv.addr()},
		Timeout:               2 * time.Second,
		AllowPrivateResolvers: true,
	})

	for _, q := range report.Resolvers[0].Queries {
		if q.FailureClass != DNSFailRefused {
			t.Fatalf("failure class = %s, want refused", q.FailureClass)
		}
	}
}

// TestDNSDiagnosticServfail verifies the SERVFAIL classification.
func TestDNSDiagnosticServfail(t *testing.T) {
	srv := startFakeDNS(t, func(_ string, query []byte) []byte {
		return fakeDNSResponse(query, 2, false, nil) // SERVFAIL
	})

	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers:             []string{srv.addr()},
		Timeout:               2 * time.Second,
		AllowPrivateResolvers: true,
	})

	for _, q := range report.Resolvers[0].Queries {
		if q.FailureClass != DNSFailServfail {
			t.Fatalf("failure class = %s, want servfail", q.FailureClass)
		}
	}
}

// TestDNSDiagnosticEmptyAnswer verifies the empty-answer
// classification (NOERROR, no matching records).
func TestDNSDiagnosticEmptyAnswer(t *testing.T) {
	srv := startFakeDNS(t, func(_ string, query []byte) []byte {
		return fakeDNSResponse(query, 0, false, nil) // NOERROR, zero answers
	})

	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers:             []string{srv.addr()},
		Timeout:               2 * time.Second,
		AllowPrivateResolvers: true,
	})

	for _, q := range report.Resolvers[0].Queries {
		if q.FailureClass != DNSFailEmpty {
			t.Fatalf("failure class = %s, want empty_answer", q.FailureClass)
		}
	}
}

// TestDNSDiagnosticTCFallbackTCP verifies a truncated UDP response
// drives the client to TCP and the row records the tcp transport.
func TestDNSDiagnosticTCFallbackTCP(t *testing.T) {
	sawTCP := false

	srv := startFakeDNS(t, func(transport string, query []byte) []byte {
		if len(query) < 14 {
			return nil
		}

		qtype := binary.BigEndian.Uint16(query[len(query)-4 : len(query)-2])

		if qtype != 1 && qtype != 28 {
			return nil
		}

		ip := net.ParseIP("203.0.113.30")
		if qtype == 28 {
			ip = net.ParseIP("2001:db8::30")
		}

		answers := [][]byte{dnsAnswerIP("example.com", qtype, ip)}

		if transport == "udp" {
			// Truncated: the client must fall back to TCP.
			return fakeDNSResponse(query, 0, true, nil)
		}

		sawTCP = true

		return fakeDNSResponse(query, 0, false, answers)
	})

	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers:             []string{srv.addr()},
		Timeout:               2 * time.Second,
		AllowPrivateResolvers: true,
	})

	if len(report.Resolvers) != 1 {
		t.Fatalf("resolver rows = %d", len(report.Resolvers))
	}

	for _, q := range report.Resolvers[0].Queries {
		if !q.OK {
			t.Fatalf("query failed: %+v", q)
		}

		if q.Transport != "tcp" {
			t.Fatalf("transport = %s, want tcp (fallback)", q.Transport)
		}
	}

	if !sawTCP {
		t.Fatal("the fake never saw the TCP fallback")
	}
}

// TestDNSDiagnosticTimeout verifies the timeout classification
// against a silent black-hole resolver.
func TestDNSDiagnosticTimeout(t *testing.T) {
	srv := startFakeDNS(t, func(_ string, query []byte) []byte {
		return nil // silent
	})

	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers:             []string{srv.addr()},
		Timeout:               500 * time.Millisecond,
		AllowPrivateResolvers: true,
	})

	for _, q := range report.Resolvers[0].Queries {
		if q.FailureClass != DNSFailTimeout {
			t.Fatalf("failure class = %s, want timeout (err=%s)", q.FailureClass, q.Error)
		}

		if q.OK {
			t.Fatal("silent resolver must not be OK")
		}
	}
}

// TestDNSDiagnosticMalformed verifies malformed responses are
// classified, never trusted.
func TestDNSDiagnosticMalformed(t *testing.T) {
	srv := startFakeDNS(t, func(_ string, query []byte) []byte {
		return []byte("this is not dns at all")
	})

	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers:             []string{srv.addr()},
		Timeout:               2 * time.Second,
		AllowPrivateResolvers: true,
	})

	for _, q := range report.Resolvers[0].Queries {
		if q.FailureClass != DNSFailMalformed {
			t.Fatalf("failure class = %s, want malformed (err=%s)", q.FailureClass, q.Error)
		}
	}
}

// TestDNSDiagnosticCancellation verifies propagation.
func TestDNSDiagnosticCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	startFakeDNS(t, func(_ string, query []byte) []byte { return nil })

	report := RunDNSDiagnostic(ctx, DNSDiagnosticOptions{
		Resolvers:             []string{"127.0.0.1"},
		AllowPrivateResolvers: true,
		Timeout:               2 * time.Second,
	})

	if !report.Cancelled {
		t.Fatal("cancelled context must mark the report cancelled")
	}
}

// TestDNSDiagnosticPrivateResolverRejected verifies the private-target
// policy (the tool surface never enables the test capability).
func TestDNSDiagnosticPrivateResolverRejected(t *testing.T) {
	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers: []string{"192.168.1.1"},
		Timeout:   1 * time.Second,
	})

	row := report.Resolvers[0]

	if row.OK {
		t.Fatal("private resolver must be rejected")
	}

	for _, q := range row.Queries {
		if q.FailureClass != DNSFailInvalid {
			t.Fatalf("failure class = %s, want invalid_target", q.FailureClass)
		}

		if q.Transport != "none" {
			t.Fatalf("rejected resolver must not probe (transport %s)", q.Transport)
		}
	}
}

// TestDNSDiagnosticInvalidTarget verifies query-name validation.
func TestDNSDiagnosticInvalidTarget(t *testing.T) {
	for _, name := range []string{
		"https://example.com/",
		"192.0.2.1",
		strings.Repeat("a", 300),
		"bad..name",
	} {
		report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
			Name:    name,
			Timeout: 1 * time.Second,
		})

		if len(report.Resolvers) == 0 {
			t.Fatalf("name %q produced no rows", name)
		}

		for _, q := range report.Resolvers[0].Queries {
			if q.FailureClass != DNSFailInvalid {
				t.Fatalf("name %q: failure class = %s, want invalid_target", name, q.FailureClass)
			}
		}
	}
}

// TestValidateDNSQueryNameAcceptsGoodNames guards against
// over-validation.
func TestValidateDNSQueryNameAcceptsGoodNames(t *testing.T) {
	for _, name := range []string{
		"example.com",
		"www.gstatic.com",
		"a-b.example.co.uk",
		"localhost",
	} {
		if err := ValidateDNSQueryName(name); err != nil {
			t.Fatalf("valid name %q rejected: %v", name, err)
		}
	}
}

// TestDNSDiagnosticDeterministicOutput verifies the report shape is
// stable: same inputs, same rows in the same order (no map iteration
// leaks).
func TestDNSDiagnosticDeterministicOutput(t *testing.T) {
	run := func() []string {
		report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
			Resolvers:             []string{"203.0.113.53", "203.0.113.54"},
			Timeout:               300 * time.Millisecond,
			AllowPrivateResolvers: true, // both are TEST-NET — still deterministic
		})

		labels := make([]string, 0, len(report.Resolvers))
		for _, row := range report.Resolvers {
			labels = append(labels, row.Resolver)
		}

		return labels
	}

	first := run()
	if len(first) != 2 {
		t.Fatalf("resolver rows = %d, want 2", len(first))
	}

	for i := 0; i < 5; i++ {
		next := run()
		if fmt.Sprint(next) != fmt.Sprint(first) {
			t.Fatalf("resolver order not deterministic: %v vs %v", next, first)
		}
	}
}

// TestDNSDiagnosticBoundedConcurrency is structural: the curated set
// is small and the resolver bound enforced (a run with an oversized
// user set is truncated, never fan-out).
func TestDNSDiagnosticBoundedResolvers(t *testing.T) {
	resolvers := []string{"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4", "203.0.113.5", "203.0.113.6", "203.0.113.7", "203.0.113.8"}

	report := RunDNSDiagnostic(context.Background(), DNSDiagnosticOptions{
		Resolvers:             resolvers,
		Timeout:               100 * time.Millisecond,
		AllowPrivateResolvers: true,
	})

	if len(report.Resolvers) > MaxDNSDiagnosticResolvers {
		t.Fatalf("resolver rows = %d, bound is %d", len(report.Resolvers), MaxDNSDiagnosticResolvers)
	}
}
