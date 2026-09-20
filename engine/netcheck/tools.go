// tools.go implements the v0.9.8.1 shared Internet-Tools engine
// (§5 of the upgrade specification): ONE bounded, cancellable,
// structured tool architecture for every user-triggered network
// diagnostic, extending engine/netcheck instead of spawning unrelated
// services.
//
// Every tool execution shares the same contract:
//
//	tool_id / target / started_at / finished_at / duration_ms /
//	status / transport / path (direct|tunneled) / provider /
//	measurement / error / details
//
// and obeys the same hard rules (§7 network-tool safety):
//
//   - every tool is bounded by a timeout and honour cancellation;
//   - targets are validated before any bytes leave the machine;
//   - redirects are capped, response bodies are size-capped;
//   - DNS rebinding is prevented for direct probes (resolve →
//     validate → dial the validated address);
//   - private / link-local / loopback destinations are blocked for
//     autonomous targets and allowed only for explicitly local
//     diagnostics (the tunnel/endpoint tools);
//   - remote JavaScript is never executed and configuration
//     credentials are never transmitted;
//   - tools run ONLY on explicit user action — never automatically
//     at startup (the service layer enforces this);
//   - structured, credential-free events are emitted for every run.
package netcheck

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

// Subsystem identifies the diagnostics layer in structured logs.
// (Redeclared here for the tools layer's doc; the canonical const
// lives in netcheck.go.)

// ToolID identifies one user-triggered network tool.
type ToolID string

// The tool catalogue (§6). Every entry is user-triggered only.
const (
	// Connectivity.
	ToolInternet ToolID = "internet" // aggregate connectivity check (the classic Report)
	ToolDNS      ToolID = "dns"      // resolve a name through a resolver
	ToolTCP      ToolID = "tcp"      // raw TCP reachability + RTT
	ToolTLS      ToolID = "tls"      // TCP + TLS handshake
	ToolHTTPS    ToolID = "https"    // full HTTP(S) GET with redirects/size caps

	// Protocol.
	ToolHTTPConnect ToolID = "http_connect" // HTTP proxy CONNECT test
	ToolSOCKS5      ToolID = "socks5"       // SOCKS5 CONNECT test
	ToolWebSocket   ToolID = "websocket"    // WebSocket upgrade handshake
	ToolUDP         ToolID = "udp"          // UDP payload round trip (DNS query)
	ToolQUIC        ToolID = "quic"         // QUIC — honestly reported below

	// Path.
	ToolTraceroute    ToolID = "traceroute"     // ICMP TTL walk (privilege-gated)
	ToolPathMTU       ToolID = "path_mtu"       // unfragmented payload ladder
	ToolCaptivePortal ToolID = "captive_portal" // captive-portal detection

	// Identity.
	ToolPublicIP ToolID = "public_ip" // exit IP, direct vs tunnel

	// Tunnel diagnostics.
	ToolTunnelDiagnostics ToolID = "tunnel_diagnostics" // active provider truth
)

// AllTools is the ordered catalogue surfaced by the UI.
var AllTools = []ToolID{
	ToolInternet,
	ToolDNS,
	ToolTCP,
	ToolTLS,
	ToolHTTPS,
	ToolHTTPConnect,
	ToolSOCKS5,
	ToolWebSocket,
	ToolUDP,
	ToolQUIC,
	ToolTraceroute,
	ToolPathMTU,
	ToolCaptivePortal,
	ToolPublicIP,
	ToolTunnelDiagnostics,
}

// ToolLabel returns the human-facing name of a tool.
func ToolLabel(id ToolID) string {
	switch id {
	case ToolInternet:
		return "Internet check"
	case ToolDNS:
		return "DNS diagnostic"
	case ToolTCP:
		return "TCP"
	case ToolTLS:
		return "TLS"
	case ToolHTTPS:
		return "HTTPS"
	case ToolHTTPConnect:
		return "HTTP CONNECT"
	case ToolSOCKS5:
		return "SOCKS5"
	case ToolWebSocket:
		return "WebSocket"
	case ToolUDP:
		return "UDP"
	case ToolQUIC:
		return "QUIC"
	case ToolTraceroute:
		return "Traceroute"
	case ToolPathMTU:
		return "Path MTU"
	case ToolCaptivePortal:
		return "Captive portal"
	case ToolPublicIP:
		return "Public IP"
	case ToolTunnelDiagnostics:
		return "Tunnel diagnostics"
	default:
		return string(id)
	}
}

// ToolStatus is the structured outcome state of one tool run.
type ToolStatus string

const (
	// ToolStatusOK: the tool ran and succeeded.
	ToolStatusOK ToolStatus = "ok"

	// ToolStatusFailed: the tool ran and failed (refused, reset,
	// protocol error, HTTP >= 400 ...).
	ToolStatusFailed ToolStatus = "failed"

	// ToolStatusTimeout: the tool hit its configured deadline.
	ToolStatusTimeout ToolStatus = "timeout"

	// ToolStatusCancelled: the caller cancelled the context.
	ToolStatusCancelled ToolStatus = "cancelled"

	// ToolStatusInvalid: the safety layer rejected the target before
	// any bytes left the machine.
	ToolStatusInvalid ToolStatus = "invalid_target"

	// ToolStatusUnsupported: the capability is honestly not available
	// in this build / on this platform / without privileges. Nothing
	// is claimed that was not verified.
	ToolStatusUnsupported ToolStatus = "unsupported"
)

// ToolPath says whether a tool ran direct or through the active
// tunnel.
type ToolPath string

const (
	PathDirect   ToolPath = "direct"
	PathTunneled ToolPath = "tunneled"
)

// ToolMeasurement is the structured, tool-specific measurement.
// Millisecond projections follow the v0.9.8.1 canonical semantics
// (engine/tester/latency.go rules R3/R4): Measured is the authority;
// 0 ms + Measured = sub-millisecond.
type ToolMeasurement struct {
	// LatencyMS is the measured round-trip (dial, handshake or full
	// request, as documented per tool). Sub-ms reads 0 — see Measured.
	LatencyMS int64 `json:"latency_ms,omitempty"`

	// Measured is the authoritative measurement flag.
	Measured bool `json:"measured,omitempty"`

	// SubMS marks a measured sub-millisecond round trip ("< 1 ms").
	SubMS bool `json:"sub_ms,omitempty"`

	// Status is the protocol-level status (HTTP status code, SOCKS5
	// reply code, DNS response code).
	Status int `json:"status,omitempty"`

	// Bytes received (bounded by the safety cap).
	Bytes int64 `json:"bytes,omitempty"`

	// Addresses are resolved answers (DNS tool) or exit IPs
	// (public_ip tool) — IP literals only, never credentials.
	Addresses []string `json:"addresses,omitempty"`

	// AddressCount avoids leaking large answer sets in the UI.
	AddressCount int `json:"address_count,omitempty"`

	// TLSVersion / Cipher describe a negotiated TLS session.
	TLSVersion string `json:"tls_version,omitempty"`
	Cipher     string `json:"cipher,omitempty"`

	// HopCount / Hops carry traceroute results (hop IPs).
	HopCount int      `json:"hop_count,omitempty"`
	Hops     []string `json:"hops,omitempty"`

	// MTUBytes is the verified unfragmented payload size (path_mtu).
	MTUBytes int `json:"mtu_bytes,omitempty"`

	// ExitIPDirect / ExitIPTunnel / ExitIPMatch answer the public_ip
	// identity questions (direct vs active tunnel).
	ExitIPDirect string `json:"exit_ip_direct,omitempty"`
	ExitIPTunnel string `json:"exit_ip_tunnel,omitempty"`
	ExitIPMatch  bool   `json:"exit_ip_match,omitempty"`

	// CaptiveDetected / CaptiveRedirect mark portal findings.
	CaptiveDetected bool   `json:"captive_detected,omitempty"`
	CaptiveRedirect string `json:"captive_redirect,omitempty"`

	// Tunnel carries the active-tunnel truth (tunnel_diagnostics).
	Tunnel *TunnelSnapshot `json:"tunnel,omitempty"`

	// Probes counts the bounded sub-probes a multi-target tool ran.
	Probes int `json:"probes,omitempty"`
}

// TunnelSnapshot is the live, measured truth about the active tunnel
// supplied by the caller (never fabricated by the tools layer).
type TunnelSnapshot struct {
	Active    bool   `json:"active"`
	Provider  string `json:"provider,omitempty"` // "xray" / "v2ray" / "sing-box" / "tor" / "psiphon"
	Endpoint  string `json:"endpoint,omitempty"` // local SOCKS host:port
	Healthy   bool   `json:"healthy,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Details   string `json:"details,omitempty"`
}

// ToolResult is the complete structured outcome of one tool run.
type ToolResult struct {
	ToolID      ToolID            `json:"tool_id"`
	Target      string            `json:"target,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  time.Time         `json:"finished_at"`
	DurationMS  int64             `json:"duration_ms"`
	Status      ToolStatus        `json:"status"`
	Transport   string            `json:"transport,omitempty"` // tcp/udp/tls/https/icmp/ws/socks5/http-connect
	Path        ToolPath          `json:"path,omitempty"`
	Provider    string            `json:"provider,omitempty"` // "" = direct/system
	Measurement ToolMeasurement   `json:"measurement"`
	Error       string            `json:"error,omitempty"`
	Details     map[string]string `json:"details,omitempty"`

	// DNS carries the structured DNS-diagnostic evidence (§5) when
	// this run is a DNS diagnostic (v0.9.8.5) — per-resolver rows
	// with per-record-type results, transports and failure classes.
	DNS *DNSDiagnosticReport `json:"dns,omitempty"`
}

// OK reports whether the tool succeeded.
func (r ToolResult) OK() bool { return r.Status == ToolStatusOK }

// ToolRequest is one tool invocation.
type ToolRequest struct {
	// Tool selects the tool.
	Tool ToolID

	// Target is the tool-specific target (host:port, URL, resolver).
	// Empty selects the tool's safe default.
	Target string

	// Timeout bounds the whole tool run (default per tool, minimum
	// 1s, hard cap 60s — no unbounded probing).
	Timeout time.Duration

	// Path selects direct or tunneled execution. Tunneled requires
	// Dial and a Provider label.
	Path ToolPath

	// Dial connects through the active tunnel (socks5.Dialer shape).
	// The service layer supplies the LIVE endpoint's dialer.
	Dial DialFunc

	// Provider labels the tunneled path's provider for reporting.
	Provider string

	// Tunnel supplies live tunnel state for tunnel_diagnostics.
	Tunnel *TunnelSnapshot
}

// DialFunc connects to host:port on behalf of a tool. The socks5
// dialer satisfies it.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Default tool timeouts (bounded by design; none exceed 60s).
const (
	DefaultToolTimeout       = 10 * time.Second
	DefaultInternetTimeout   = 20 * time.Second
	DefaultTracerouteTimeout = 30 * time.Second
	DefaultMTUTimeout        = 15 * time.Second
	DefaultPublicIPTimeout   = 15 * time.Second
	MaxToolTimeout           = 60 * time.Second
)

// ToolRunner executes tools with the shared safety policy. It is
// stateless and safe for concurrent use; bounding concurrency across
// simultaneous tool runs is the service layer's job (semaphore).
type ToolRunner struct {
	Safety Safety
}

// NewToolRunner creates a runner with the default safety policy.
func NewToolRunner() *ToolRunner {
	return &ToolRunner{Safety: DefaultSafety()}
}

// Run executes one tool under the contract: validate → bound → run →
// structure → log. It NEVER panics across tool bugs and always
// returns a ToolResult.
func (r *ToolRunner) Run(ctx context.Context, req ToolRequest) ToolResult {
	started := time.Now().UTC()

	result := ToolResult{
		ToolID:    req.Tool,
		StartedAt: started,
		Path:      effectivePath(req),
		Provider:  req.Provider,
	}

	if req.Path == PathTunneled && req.Dial == nil {
		// A tunneled run without a live dial is honestly reported,
		// never silently downgraded to a direct probe.
		result.Status = ToolStatusUnsupported
		result.Error = "no active tunnel"
		finishTool(&result, started)
		return result
	}

	timeout := normalizedToolTimeout(req.Tool, req.Timeout)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logToolStart(req, timeout)

	// Safety validation BEFORE any bytes leave the machine.
	safety := r.Safety
	if err := safety.ValidateToolTarget(req); err != nil {
		result.Status = ToolStatusInvalid
		result.Error = err.Error()
		finishTool(&result, started)
		logToolResult(result)
		return result
	}

	defer func() {
		if p := recover(); p != nil {
			result.Status = ToolStatusFailed
			result.Error = fmt.Sprintf("tool panicked: %v", p)
			finishTool(&result, started)
			logToolResult(result)
		}
	}()

	result.Target = displayTarget(req)

	switch req.Tool {
	case ToolInternet:
		r.runInternet(ctx, req, &result)
	case ToolDNS:
		r.runDNS(ctx, req, &result)
	case ToolTCP:
		r.runTCP(ctx, req, &result)
	case ToolTLS:
		r.runTLS(ctx, req, &result)
	case ToolHTTPS:
		r.runHTTPS(ctx, req, &result)
	case ToolHTTPConnect:
		r.runHTTPConnect(ctx, req, &result)
	case ToolSOCKS5:
		r.runSOCKS5(ctx, req, &result)
	case ToolWebSocket:
		r.runWebSocket(ctx, req, &result)
	case ToolUDP:
		r.runUDP(ctx, req, &result)
	case ToolQUIC:
		// Honest capability report: this build carries no QUIC
		// transport stack. UDP reachability is covered by ToolUDP.
		result.Status = ToolStatusUnsupported
		result.Transport = "quic"
		result.Error = "QUIC handshake probing is not compiled into this build; use the UDP tool for datagram reachability"
	case ToolTraceroute:
		r.runTraceroute(ctx, req, &result)
	case ToolPathMTU:
		r.runPathMTU(ctx, req, &result)
	case ToolCaptivePortal:
		r.runCaptivePortal(ctx, req, &result)
	case ToolPublicIP:
		r.runPublicIP(ctx, req, &result)
	case ToolTunnelDiagnostics:
		r.runTunnelDiagnostics(req, &result)
	default:
		result.Status = ToolStatusInvalid
		result.Error = fmt.Sprintf("unknown tool %q", req.Tool)
	}

	finishTool(&result, started)
	logToolResult(result)

	return result
}

// effectivePath defaults to direct.
func effectivePath(req ToolRequest) ToolPath {
	if req.Path == PathTunneled {
		return PathTunneled
	}

	return PathDirect
}

// normalizedToolTimeout clamps the requested timeout into the
// bounded window (min 1s, per-tool default, hard cap 60s).
func normalizedToolTimeout(tool ToolID, requested time.Duration) time.Duration {
	def := DefaultToolTimeout

	switch tool {
	case ToolInternet:
		def = DefaultInternetTimeout
	case ToolTraceroute:
		def = DefaultTracerouteTimeout
	case ToolPathMTU:
		def = DefaultMTUTimeout
	case ToolPublicIP:
		def = DefaultPublicIPTimeout
	}

	if requested <= 0 {
		return def
	}

	if requested < time.Second {
		return time.Second
	}

	if requested > MaxToolTimeout {
		return MaxToolTimeout
	}

	return requested
}

// finishTool stamps completion and derives the wall-time projection.
func finishTool(result *ToolResult, started time.Time) {
	result.FinishedAt = time.Now().UTC()

	d := result.FinishedAt.Sub(started)
	if d < 0 {
		d = 0
	}

	// Wall time of the run is operational telemetry (always measured):
	// quantize sub-ms to 1 so duration_ms 0 is never ambiguous with
	// "did not run" (mirrors netcheck.elapsedMS policy).
	if ms := d.Milliseconds(); ms > 0 {
		result.DurationMS = ms
	} else {
		result.DurationMS = 1
	}
}

// displayTarget renders the target for reporting (already validated).
func displayTarget(req ToolRequest) string {
	if req.Target != "" {
		return req.Target
	}

	return defaultToolTarget(req.Tool)
}

// logToolStart emits the structured network_tool_start event.
func logToolStart(req ToolRequest, timeout time.Duration) {
	logging.LogR(logging.Record{
		Level:     logging.LevelDebug,
		Subsystem: Subsystem,
		Event:     "network_tool_start",
		Message:   ToolLabel(req.Tool) + " started",
		Status:    "running",
		Fields: map[string]any{
			"tool":     string(req.Tool),
			"timeout":  timeout.String(),
			"path":     string(effectivePath(req)),
			"provider": req.Provider,
		},
	})
}

// logToolResult emits the structured completion event. Targets are
// host:port / URL forms validated by the safety layer — never
// credentials, UUIDs or subscription URIs.
func logToolResult(result ToolResult) {
	level := logging.LevelInfo
	event := "network_tool_complete"

	if result.Status != ToolStatusOK {
		level = logging.LevelWarn
		event = "network_tool_failed"
	}

	logging.LogR(logging.Record{
		Level:      level,
		Subsystem:  Subsystem,
		Event:      event,
		Message:    ToolLabel(result.ToolID) + " " + string(result.Status),
		Status:     string(result.Status),
		DurationMS: result.DurationMS,
		Fields: map[string]any{
			"tool":       string(result.ToolID),
			"target":     result.Target,
			"transport":  result.Transport,
			"path":       string(result.Path),
			"provider":   result.Provider,
			"error_kind": toolErrorKind(result),
		},
	})
}

// toolErrorKind classifies failures for structured logging.
func toolErrorKind(result ToolResult) string {
	switch result.Status {
	case ToolStatusTimeout:
		return "timeout"
	case ToolStatusCancelled:
		return "cancelled"
	case ToolStatusInvalid:
		return "invalid_target"
	case ToolStatusUnsupported:
		return "unsupported"
	case ToolStatusFailed:
		return classifyToolError(result.Error)
	default:
		return ""
	}
}

// classifyToolError maps a raw error string to a short class.
func classifyToolError(err string) string {
	if err == "" {
		return "unknown"
	}

	lower := strings.ToLower(err)

	switch {
	case strings.Contains(lower, "refused"):
		return "refused"
	case strings.Contains(lower, "reset"):
		return "reset"
	case strings.Contains(lower, "no route"), strings.Contains(lower, "unreachable"):
		return "unreachable"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline"):
		return "timeout"
	case strings.Contains(lower, "tls"), strings.Contains(lower, "certificate"), strings.Contains(lower, "handshake"):
		return "tls"
	case strings.Contains(lower, "blocked"), strings.Contains(lower, "private"):
		return "blocked"
	default:
		return "error"
	}
}

// ToolCatalogue returns the catalogue with labels for the UI.
type ToolInfo struct {
	ID      ToolID `json:"id"`
	Label   string `json:"label"`
	Group   string `json:"group"`
	Target  bool   `json:"takes_target"` // user may supply a custom target
	Timeout int64  `json:"timeout_ms"`   // default timeout
}

// ToolCatalogue lists every tool with its UI metadata.
func ToolCatalogue() []ToolInfo {
	infos := []ToolInfo{
		{ID: ToolInternet, Label: ToolLabel(ToolInternet), Group: "connectivity"},
		{ID: ToolDNS, Label: ToolLabel(ToolDNS), Group: "connectivity", Target: true},
		{ID: ToolTCP, Label: ToolLabel(ToolTCP), Group: "connectivity", Target: true},
		{ID: ToolTLS, Label: ToolLabel(ToolTLS), Group: "connectivity", Target: true},
		{ID: ToolHTTPS, Label: ToolLabel(ToolHTTPS), Group: "connectivity", Target: true},
		{ID: ToolHTTPConnect, Label: ToolLabel(ToolHTTPConnect), Group: "protocol", Target: true},
		{ID: ToolSOCKS5, Label: ToolLabel(ToolSOCKS5), Group: "protocol", Target: true},
		{ID: ToolWebSocket, Label: ToolLabel(ToolWebSocket), Group: "protocol", Target: true},
		{ID: ToolUDP, Label: ToolLabel(ToolUDP), Group: "protocol", Target: true},
		{ID: ToolQUIC, Label: ToolLabel(ToolQUIC), Group: "protocol", Target: true},
		{ID: ToolTraceroute, Label: ToolLabel(ToolTraceroute), Group: "path", Target: true},
		{ID: ToolPathMTU, Label: ToolLabel(ToolPathMTU), Group: "path", Target: true},
		{ID: ToolCaptivePortal, Label: ToolLabel(ToolCaptivePortal), Group: "path"},
		{ID: ToolPublicIP, Label: ToolLabel(ToolPublicIP), Group: "identity"},
		{ID: ToolTunnelDiagnostics, Label: ToolLabel(ToolTunnelDiagnostics), Group: "tunnel"},
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })

	for i := range infos {
		infos[i].Timeout = int64(normalizedToolTimeout(infos[i].ID, 0) / time.Millisecond)
	}

	return infos
}
