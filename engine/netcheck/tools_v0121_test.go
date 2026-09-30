package netcheck

// tools_v0121_test.go pins the v0.12.1 status-semantics contract:
// every status class carries its OWN log event; DNS aggregates
// partial evidence honestly; the endpoint tools never probe a
// hard-coded 127.0.0.1:1080 and report not_configured instead of a
// fake failure. Fixtures are local and deterministic.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

// TestToolStatusEventMapping pins §4: one event name per status —
// never a single network_tool_failed for every non-OK outcome.
func TestToolStatusEventMapping(t *testing.T) {
	cases := []struct {
		status ToolStatus
		event  string
	}{
		{ToolStatusOK, "network_tool_complete"},
		{ToolStatusPartial, "network_tool_partial"},
		{ToolStatusFailed, "network_tool_failed"},
		{ToolStatusTimeout, "network_tool_timeout"},
		{ToolStatusCancelled, "network_tool_cancelled"},
		{ToolStatusInvalid, "network_tool_invalid"},
		{ToolStatusUnsupported, "network_tool_unsupported"},
		{ToolStatusNotConfigured, "network_tool_not_configured"},
		{ToolStatusNotApplicable, "network_tool_not_applicable"},
		{ToolStatusUnreachable, "network_tool_unreachable"},
	}

	for _, tc := range cases {
		if got := toolStatusEvent(tc.status); got != tc.event {
			t.Errorf("status %q event = %q, want %q", tc.status, got, tc.event)
		}
	}
}

// TestToolStatusLevels pins the severity contract: environment facts
// (not configured / not applicable / unsupported / cancelled / partial
// success) are informational — never warn-level noise.
func TestToolStatusLevels(t *testing.T) {
	infoStatuses := []ToolStatus{
		ToolStatusOK, ToolStatusPartial, ToolStatusUnsupported,
		ToolStatusNotConfigured, ToolStatusNotApplicable, ToolStatusCancelled,
	}

	for _, status := range infoStatuses {
		if got := toolStatusLevel(status); got != logging.LevelInfo {
			t.Errorf("status %q level = %v, want info", status, got)
		}
	}

	if got := toolStatusLevel(ToolStatusFailed); got != logging.LevelWarn {
		t.Errorf("failed level = %v, want warn", got)
	}

	if got := toolStatusLevel(ToolStatusUnreachable); got != logging.LevelWarn {
		t.Errorf("unreachable level = %v, want warn", got)
	}
}

// TestDNSAggregateStatus pins the §5 aggregation: partial when the
// evidence splits, failed only when everything applicable failed.
func TestDNSAggregateStatus(t *testing.T) {
	report := func(rows ...bool) *DNSDiagnosticReport {
		out := &DNSDiagnosticReport{}
		resolver := DNSResolverResult{Resolver: "fixture"}

		for _, ok := range rows {
			resolver.Queries = append(resolver.Queries, DNSQueryResult{
				RecordType: DNSTypeA, OK: ok,
				FailureClass: map[bool]DNSFailureClass{true: DNSFailNone, false: DNSFailTimeout}[ok],
			})
		}

		out.Resolvers = []DNSResolverResult{resolver}

		return out
	}

	// All success → ok.
	status, _, _ := dnsAggregateStatus(2, 2, false, report(true, true))
	if status != ToolStatusOK {
		t.Errorf("all success = %q, want ok", status)
	}

	// Some success + some failure → partial.
	status, _, _ = dnsAggregateStatus(2, 4, false, report(true, true, false, false))
	if status != ToolStatusPartial {
		t.Errorf("mixed = %q, want partial", status)
	}

	// A-only success (AAAA blocked) → partial.
	status, _, _ = dnsAggregateStatus(1, 2, false, report(true, false))
	if status != ToolStatusPartial {
		t.Errorf("A-only = %q, want partial", status)
	}

	// All failure → failed with the dominant class.
	var class string

	status, class, _ = dnsAggregateStatus(0, 2, false, report(false, false))
	if status != ToolStatusFailed {
		t.Errorf("all failure = %q, want failed", status)
	}

	if class != "timeout" {
		t.Errorf("dominant class = %q, want timeout", class)
	}

	// None answered + cancelled run → cancelled.
	status, _, _ = dnsAggregateStatus(0, 2, true, report(false, false))
	if status != ToolStatusCancelled {
		t.Errorf("cancelled = %q, want cancelled", status)
	}
}

// TestDNSReportRetainsSuccessfulEvidence: a partial run keeps the
// successful rows (addresses + measurements) — evidence is never
// discarded because other rows failed.
func TestDNSReportRetainsSuccessfulEvidence(t *testing.T) {
	report := &DNSDiagnosticReport{
		Resolvers: []DNSResolverResult{
			{
				Resolver: "System",
				Queries: []DNSQueryResult{
					{RecordType: DNSTypeA, OK: false, FailureClass: DNSFailTimeout, Error: "timeout"},
				},
			},
			{
				Resolver: "Cloudflare",
				Address:  "1.1.1.1",
				OK:       true,
				Queries: []DNSQueryResult{
					{RecordType: DNSTypeA, OK: true, LatencyMS: 21, Measured: true, Addresses: []string{"142.250.4.103"}, AnswerCount: 1},
					{RecordType: DNSTypeAAAA, OK: true, LatencyMS: 22, Measured: true, Addresses: []string{"2607:f8b0:4004:c07::71"}, AnswerCount: 1},
				},
			},
		},
	}

	bestLatency, bestAnswers, okQueries, totalQueries := summarizeDNSReport(report)

	if okQueries != 2 || totalQueries != 3 {
		t.Fatalf("ok/total = %d/%d, want 2/3", okQueries, totalQueries)
	}

	if bestLatency != 21 || len(bestAnswers) != 1 {
		t.Fatalf("best evidence lost: latency=%d answers=%v", bestLatency, bestAnswers)
	}
}

// TestHTTPConnectNotConfiguredWithoutEndpoint: the §6 contract — no
// configured HTTP proxy is a PREREQUISITE state, not a network
// failure, and no blind 127.0.0.1:1080 probe ever leaves the machine.
func TestHTTPConnectNotConfiguredWithoutEndpoint(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolHTTPConnect})

	if result.Status != ToolStatusNotConfigured {
		t.Fatalf("status = %q, want not_configured", result.Status)
	}

	if result.Target != "" && strings.Contains(result.Target, "127.0.0.1:1080") {
		t.Fatalf("target = %q — the hard-coded default must never reappear", result.Target)
	}
}

// TestHTTPConnectCompatibleEndpoint: a protocol-compatible local
// endpoint is used when the user gave no explicit target.
func TestHTTPConnectCompatibleEndpoint(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer proxy.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	p, err := net.LookupPort("tcp", port)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	runner := NewToolRunner()
	runner.Safety.AllowPrivateTargets = true

	result := runner.Run(context.Background(), ToolRequest{
		Tool:              ToolHTTPConnect,
		LocalEndpoint:     net.JoinHostPort(host, itoa(p)),
		LocalEndpointKind: "http",
	})

	// httptest does not implement a real CONNECT tunnel — a non-200
	// answer IS the honest evidence of a live proxy speaking HTTP.
	// The forbidden outcome is a prerequisite misclassification:
	// not_configured would mean the endpoint was never consulted.
	if result.Status == ToolStatusNotConfigured {
		t.Fatalf("status = %q — the compatible endpoint must be probed", result.Status)
	}
}

// TestHTTPConnectIncompatibleEndpointKind: a SOCKS endpoint is NOT
// offered to the HTTP CONNECT tool — wrong-kind endpoints fall
// through to not_configured.
func TestHTTPConnectIncompatibleEndpointKind(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:              ToolHTTPConnect,
		LocalEndpoint:     "127.0.0.1:1080",
		LocalEndpointKind: "socks5",
	})

	if result.Status != ToolStatusNotConfigured {
		t.Fatalf("status = %q, want not_configured", result.Status)
	}
}

// TestSOCKS5NotConfiguredWithoutEndpoint: the §7 contract — without a
// session endpoint and without an explicit target, the tool reports
// not_configured and never invents 127.0.0.1:1080.
func TestSOCKS5NotConfiguredWithoutEndpoint(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolSOCKS5})

	if result.Status != ToolStatusNotConfigured {
		t.Fatalf("status = %q, want not_configured", result.Status)
	}
}

// TestWebSocketAggregatePartial pins §9: one endpoint succeeding and
// one failing aggregates to partial, with per-target evidence kept.
func TestWebSocketAggregatePartial(t *testing.T) {
	good := httpServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		w.WriteHeader(http.StatusSwitchingProtocols)
	})
	defer good.Close()

	dead := deadListenerAddr(t)

	goodURL := "ws" + strings.TrimPrefix(good.URL, "http")
	deadURL := "ws://" + dead

	// The curated set is a package variable for the same reason as
	// resolveIPAddrs: deterministic fixtures override it in tests.
	original := curatedWebSocketTargets
	curatedWebSocketTargets = []string{goodURL, deadURL}

	defer func() { curatedWebSocketTargets = original }()

	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{Tool: ToolWebSocket})

	if result.Status != ToolStatusPartial {
		t.Fatalf("status = %q (%s), want partial", result.Status, result.Error)
	}

	if !strings.Contains(result.Details["targets"], goodURL) ||
		!strings.Contains(result.Details["targets"], deadURL) {
		t.Fatalf("per-target evidence missing: %q", result.Details["targets"])
	}
}

// TestWebSocketExplicitTargetIsScoped: an explicit failing target
// fails FOR THAT TARGET only — no fallback set runs.
func TestWebSocketExplicitTargetIsScoped(t *testing.T) {
	dead := deadListenerAddr(t)

	runner := localEndpointRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool:   ToolWebSocket,
		Target: "ws://" + dead,
	})

	if result.Status != ToolStatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}

	if result.Measurement.Probes != 1 {
		t.Fatalf("probes = %d, want 1 (no fallback on explicit target)", result.Measurement.Probes)
	}
}

// TestTunnelDiagnosticsDirectNotApplicable pins §11: Direct + no
// active tunnel must never render as a failure.
func TestTunnelDiagnosticsDirectNotApplicable(t *testing.T) {
	runner := NewToolRunner()

	result := runner.Run(context.Background(), ToolRequest{
		Tool: ToolTunnelDiagnostics,
		Path: PathDirect,
	})

	if result.Status != ToolStatusNotApplicable {
		t.Fatalf("status = %q, want not_applicable", result.Status)
	}

	if result.Transport != "" {
		t.Errorf("transport = %q, want empty (no probe ran)", result.Transport)
	}
}

// deadListenerAddr returns a closed loopback TCP address — a
// deterministic "connection refused" fixture.
func deadListenerAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}

	addr := listener.Addr().String()
	listener.Close()

	return addr
}

// itoa avoids importing strconv for one call in this file.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}

	digits := ""

	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}

	return digits
}
