// stages.go implements the v0.9.8.5 staged diagnostics ladder (§4):
// one coherent, ordered model of HOW the machine reaches the Internet,
// so a failure surfaces as "which stage failed and why" instead of a
// single generic "no internet".
//
//	local link → local IP → DNS → TCP → TLS → HTTPS →
//	captive portal → direct Internet → tunnel Internet
//
// Every stage carries structured evidence: status, measured latency,
// the target that was probed and a failure class. The aggregate
// seven-state classifier (netcheck.go) remains authoritative and
// unchanged — the ladder is additive evidence, not a second verdict.
package netcheck

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// StageID identifies one stage of the diagnostic ladder.
type StageID string

// The ladder, in order.
const (
	StageLocalLink StageID = "local_link"
	StageLocalIP   StageID = "local_ip"
	StageDNS       StageID = "dns"
	StageTCP       StageID = "tcp"
	StageTLS       StageID = "tls"
	StageHTTPS     StageID = "https"
	StageCaptive   StageID = "captive_portal"
	StageDirect    StageID = "direct_internet"
	StageTunnel    StageID = "tunnel_internet"
)

// StageOrder is the canonical ladder order (assembly + reporting).
var StageOrder = []StageID{
	StageLocalLink,
	StageLocalIP,
	StageDNS,
	StageTCP,
	StageTLS,
	StageHTTPS,
	StageCaptive,
	StageDirect,
	StageTunnel,
}

// StageLabel renders the human-facing name of a stage.
func StageLabel(s StageID) string {
	switch s {
	case StageLocalLink:
		return "Local link"
	case StageLocalIP:
		return "Local IP"
	case StageDNS:
		return "DNS"
	case StageTCP:
		return "TCP"
	case StageTLS:
		return "TLS"
	case StageHTTPS:
		return "HTTPS"
	case StageCaptive:
		return "Captive portal"
	case StageDirect:
		return "Direct Internet"
	case StageTunnel:
		return "Tunnel Internet"
	default:
		return string(s)
	}
}

// StageStatus is the outcome of one ladder stage.
type StageStatus string

const (
	// StageOK: the stage succeeded (measured).
	StageOK StageStatus = "ok"

	// StageFailed: the stage was checked and failed.
	StageFailed StageStatus = "failed"

	// StageSkipped: the stage could not be meaningfully evaluated
	// because an earlier stage already failed (e.g. captive portal
	// cannot be judged when HTTPS is dead).
	StageSkipped StageStatus = "skipped"

	// StageNotChecked: the stage was not part of this run (e.g. the
	// tunnel stage without a proxy address).
	StageNotChecked StageStatus = "not_checked"
)

// StageResult is the structured evidence of ONE ladder stage.
type StageResult struct {
	Stage        StageID     `json:"stage"`
	Status       StageStatus `json:"status"`
	LatencyMS    int64       `json:"latency_ms,omitempty"`
	Measured     bool        `json:"measured,omitempty"`
	Target       string      `json:"target,omitempty"`
	Detail       string      `json:"detail,omitempty"`
	FailureClass string      `json:"failure_class,omitempty"`
}

// FirstFailedStage returns the first failed stage of the ladder (nil
// when every checked stage passed) — the "which stage failed" answer.
func FirstFailedStage(stages []StageResult) *StageResult {
	index := make(map[StageID]StageResult, len(stages))
	for _, s := range stages {
		index[s.Stage] = s
	}

	for _, id := range StageOrder {
		if s, ok := index[id]; ok && s.Status == StageFailed {
			return &s
		}
	}

	return nil
}

// ---- stage probes ------------------------------------------------------

// localAddressEvidence is the measured local network identity: the
// route-relevant source address plus the interface that owns it.
type localAddressEvidence struct {
	Address   string
	Interface string
}

// probeLocalAddress discovers the route-relevant local source address
// WITHOUT sending a packet: a connected UDP socket performs only a
// routing-table lookup on every supported platform. The probe target
// set is the same public anycast anchors the TCP stage uses.
func probeLocalAddress(ctx context.Context) (localAddressEvidence, error) {
	anchors := []string{"8.8.8.8:443", "1.1.1.1:443", "9.9.9.9:443"}

	var lastErr error

	for _, anchor := range anchors {
		if ctx.Err() != nil {
			break
		}

		dialer := &net.Dialer{Timeout: 2 * time.Second}

		conn, err := dialer.DialContext(ctx, "udp", anchor)
		if err != nil {
			lastErr = err

			continue
		}

		local := conn.LocalAddr().String()
		_ = conn.Close()

		host, _, err := net.SplitHostPort(local)
		if err != nil || host == "" {
			continue
		}

		iface := interfaceOfAddress(host)

		return localAddressEvidence{Address: host, Interface: iface}, nil
	}

	if lastErr != nil {
		return localAddressEvidence{}, fmt.Errorf("no route to any public anchor: %v", lastErr)
	}

	return localAddressEvidence{}, fmt.Errorf("no route to any public anchor")
}

// probeLocalAddress6 mirrors probeLocalAddress for the IPv6 default
// route. A machine with no IPv6 route reports an honest failure (the
// stage is skipped for classification purposes, never fabricated).
func probeLocalAddress6(ctx context.Context) (localAddressEvidence, error) {
	anchors := []string{"[2001:4860:4860::8888]:443", "[2606:4700:4700::1111]:443"}

	for _, anchor := range anchors {
		if ctx.Err() != nil {
			break
		}

		dialer := &net.Dialer{Timeout: 2 * time.Second}

		conn, err := dialer.DialContext(ctx, "udp", anchor)
		if err != nil {
			continue
		}

		local := conn.LocalAddr().String()
		_ = conn.Close()

		host, _, err := net.SplitHostPort(local)
		if err != nil || host == "" {
			continue
		}

		iface := interfaceOfAddress(host)

		return localAddressEvidence{Address: host, Interface: iface}, nil
	}

	return localAddressEvidence{}, fmt.Errorf("no IPv6 default route")
}

// interfaceOfAddress finds the interface name owning one address.
func interfaceOfAddress(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil {
		return ""
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, a := range addrs {
			var ipnet *net.IPNet

			switch v := a.(type) {
			case *net.IPNet:
				ipnet = v
			case *net.IPAddr:
				ipnet = &net.IPNet{IP: v.IP, Mask: net.CIDRMask(len(v.IP)*8, len(v.IP)*8)}
			}

			if ipnet != nil && ipnet.IP.Equal(ip) {
				return iface.Name
			}
		}
	}

	return ""
}

// tlsStageEvidence is the measured TLS handshake outcome.
type tlsStageEvidence struct {
	LatencyMS int64
	Version   string
	Err       error
}

// probeTLSStage performs one real TLS handshake against the probe
// host and reports the measured handshake latency plus the negotiated
// protocol version (the "TLS" ladder stage).
func probeTLSStage(ctx context.Context, host string) tlsStageEvidence {
	addr := net.JoinHostPort(host, "443")

	dialer := &net.Dialer{Timeout: 5 * time.Second}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return tlsStageEvidence{Err: err}
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})

	handshakeStart := time.Now()

	handshakeErr := tlsConn.HandshakeContext(ctx)
	elapsed := time.Since(handshakeStart)

	if handshakeErr != nil {
		_ = tlsConn.Close()

		return tlsStageEvidence{Err: handshakeErr}
	}

	state := tlsConn.ConnectionState()
	_ = tlsConn.Close()

	return tlsStageEvidence{
		LatencyMS: maxI64(elapsed.Milliseconds(), 1),
		Version:   tlsVersionLabel(state.Version),
	}
}

// captiveStageEvidence is the measured captive-portal outcome.
type captiveStageEvidence struct {
	Detected bool
	Redirect string
	Checked  bool
	Err      error
}

// probeCaptiveStage checks the classic portal signatures against one
// 204 endpoint: a redirect, a non-204/200 status, or a 200 that
// returns an HTML body. It reuses the same detection logic the
// captive-portal tool established (tools_path.go).
func probeCaptiveStage(ctx context.Context, target string) captiveStageEvidence {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 4 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // observe, never follow
		},
	}

	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return captiveStageEvidence{Err: err}
	}

	resp, err := client.Do(req)
	if err != nil {
		return captiveStageEvidence{Err: err}
	}

	defer resp.Body.Close()

	evidence := captiveStageEvidence{Checked: true}

	finalURL := resp.Request.URL.String()

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		evidence.Detected = true
		evidence.Redirect = boundedString(finalURL, 200)
	case resp.StatusCode == 200:
		head := make([]byte, 256)

		n, _ := resp.Body.Read(head)

		if isHTMLSnippet(head[:n]) {
			evidence.Detected = true
			evidence.Redirect = boundedString(finalURL, 200)
		}
	case resp.StatusCode == 204:
		// clean
	default:
		evidence.Detected = true
		evidence.Redirect = boundedString(finalURL, 200)
	}

	return evidence
}

// ---- stage assembly ----------------------------------------------------

// classifyStageError maps a probe error onto a short failure class
// (shared vocabulary with the tools layer).
func classifyStageError(err error) string {
	if err == nil {
		return ""
	}

	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "timeout"
	case strings.Contains(msg, "refused"):
		return "refused"
	case strings.Contains(msg, "reset"):
		return "reset"
	case strings.Contains(msg, "no route"), strings.Contains(msg, "unreachable"):
		return "unreachable"
	case strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"), strings.Contains(msg, "handshake"):
		return "tls"
	case strings.Contains(msg, "no such host"):
		return "dns"
	default:
		return "error"
	}
}

// boundedDetail renders a bounded detail line.
func boundedDetail(err error) string {
	if err == nil {
		return ""
	}

	return boundedString(err.Error(), 160)
}

// buildStages assembles the ladder evidence from one report's probe
// results plus the dedicated stage probes. It NEVER reclassifies the
// seven-state verdict — it explains it.
func buildStages(
	report Report,
	localIP localAddressEvidence,
	localIP6 localAddressEvidence,
	tlsEvidence tlsStageEvidence,
	captive captiveStageEvidence,
	probeHost string,
) []StageResult {
	stages := make([]StageResult, 0, len(StageOrder))

	dnsOK := countOK(report.DNS)
	tcpOK := countOK(report.TCP)
	httpsOK := countOK(report.HTTPS)

	// Local link.
	linkOK := anyOK(report.LocalLinks)

	stages = append(stages, stageFor(StageLocalLink, linkOK,
		"", "", linkOK, ""))

	// Local IP (route-relevant address).
	if localIP.Address != "" {
		detail := localIP.Address
		if localIP.Interface != "" {
			detail += " via " + localIP.Interface
		}

		if localIP6.Address != "" {
			detail += " · IPv6 " + localIP6.Address
		}

		stages = append(stages, StageResult{
			Stage: StageLocalIP, Status: StageOK, Measured: true, Detail: detail,
		})
	} else {
		stages = append(stages, StageResult{
			Stage: StageLocalIP, Status: StageFailed,
			Detail:       "no route-relevant local address (no default route?)",
			FailureClass: "unreachable",
		})
	}

	// DNS.
	if dnsOK > 0 {
		stages = append(stages, stageWithLatency(StageDNS, true,
			report.DNS, strconv.Itoa(dnsOK)+"/"+strconv.Itoa(len(report.DNS))+" resolvers"))
	} else {
		stages = append(stages, failedStageFromResults(StageDNS, report.DNS))
	}

	// TCP.
	if tcpOK > 0 {
		stages = append(stages, stageWithLatency(StageTCP, true,
			report.TCP, strconv.Itoa(tcpOK)+"/"+strconv.Itoa(len(report.TCP))+" endpoints"))
	} else {
		stages = append(stages, failedStageFromResults(StageTCP, report.TCP))
	}

	// TLS.
	if tlsEvidence.Err == nil {
		stages = append(stages, StageResult{
			Stage: StageTLS, Status: StageOK, Measured: true,
			LatencyMS: tlsEvidence.LatencyMS,
			Target:    probeHost,
			Detail:    tlsEvidence.Version,
		})
	} else {
		stages = append(stages, StageResult{
			Stage: StageTLS, Status: StageFailed,
			Target:       probeHost,
			Detail:       boundedDetail(tlsEvidence.Err),
			FailureClass: classifyStageError(tlsEvidence.Err),
		})
	}

	// HTTPS.
	if httpsOK > 0 {
		stages = append(stages, stageWithLatency(StageHTTPS, true,
			report.HTTPS, strconv.Itoa(httpsOK)+"/"+strconv.Itoa(len(report.HTTPS))+" targets"))
	} else {
		stages = append(stages, failedStageFromResults(StageHTTPS, report.HTTPS))
	}

	// Captive portal: only meaningful when HTTPS itself works —
	// otherwise the portal verdict cannot be distinguished from the
	// HTTPS failure and the stage is honestly skipped.
	switch {
	case httpsOK == 0:
		stages = append(stages, StageResult{
			Stage: StageCaptive, Status: StageSkipped,
			Detail: "HTTPS stage failed; portal state not evaluable",
		})
	case captive.Detected:
		stages = append(stages, StageResult{
			Stage: StageCaptive, Status: StageFailed,
			Detail:       "captive portal detected",
			FailureClass: "captive_portal",
			Target:       boundedString(captive.Redirect, 120),
		})
	case captive.Err != nil:
		stages = append(stages, StageResult{
			Stage: StageCaptive, Status: StageSkipped,
			Detail:       "portal probe failed: " + boundedDetail(captive.Err),
			FailureClass: classifyStageError(captive.Err),
		})
	default:
		stages = append(stages, StageResult{
			Stage: StageCaptive, Status: StageOK, Detail: "not detected",
		})
	}

	// Direct Internet: the HTTPS evidence, portal-aware.
	switch {
	case httpsOK > 0 && !captive.Detected:
		stages = append(stages, StageResult{
			Stage: StageDirect, Status: StageOK, Measured: true,
			Detail: "working directly",
		})
	case httpsOK > 0 && captive.Detected:
		stages = append(stages, StageResult{
			Stage: StageDirect, Status: StageFailed,
			Detail: "behind a captive portal", FailureClass: "captive_portal",
		})
	default:
		stages = append(stages, StageResult{
			Stage: StageDirect, Status: StageFailed,
			Detail: "no working direct HTTPS path", FailureClass: "https",
		})
	}

	// Tunnel Internet: only checked when a proxy endpoint was probed.
	if report.Proxy == nil {
		stages = append(stages, StageResult{
			Stage: StageTunnel, Status: StageNotChecked,
			Detail: "no active session endpoint",
		})
	} else if report.Proxy.OK {
		stages = append(stages, stageWithLatency(StageTunnel, true,
			[]CheckResult{*report.Proxy}, "usable through the active session"))
	} else {
		stages = append(stages, failedStageFromResults(StageTunnel, []CheckResult{*report.Proxy}))
	}

	return stages
}

// stageFor renders a boolean stage.
func stageFor(id StageID, ok bool, target, detail string, measured bool, _ string) StageResult {
	status := StageFailed
	failure := "error"

	if ok {
		status = StageOK
		failure = ""
	}

	return StageResult{
		Stage: id, Status: status, Measured: measured && ok,
		Target: target, Detail: detail, FailureClass: failure,
	}
}

// stageWithLatency renders a stage from its best successful probe.
func stageWithLatency(id StageID, ok bool, results []CheckResult, detail string) StageResult {
	best := bestLatency(results)

	return StageResult{
		Stage: id, Status: StageOK, Measured: true,
		LatencyMS: best,
		Detail:    detail,
	}
}

// failedStageFromResults renders a failed stage from its probe set,
// carrying the first concrete failure reason (which target, why).
func failedStageFromResults(id StageID, results []CheckResult) StageResult {
	stage := StageResult{Stage: id, Status: StageFailed}

	for _, r := range results {
		if r.OK || r.Error == "" {
			continue
		}

		stage.Target = r.Name
		stage.Detail = boundedString(r.Error, 160)
		stage.FailureClass = classifyStageError(fmt.Errorf("%s", r.Error))

		break
	}

	if stage.Detail == "" {
		stage.Detail = "all probes failed without a reason"
	}

	return stage
}

// countOK counts successful probes.
func countOK(list []CheckResult) int {
	n := 0

	for _, r := range list {
		if r.OK {
			n++
		}
	}

	return n
}
