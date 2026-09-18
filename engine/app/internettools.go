// internettools.go exposes the shared Internet-Tools engine (§6) to
// the UI. Hard rules enforced here:
//
//   - tools run ONLY on explicit user action — nothing outbound
//     (especially public-IP lookups) ever runs automatically at
//     startup or in the background;
//   - bounded concurrency: at most a few tools run at once (§14), so
//     tool usage can never scale into uncontrolled probing;
//   - tunneled runs are routed through the LIVE session endpoint
//     (core instance or provider session) and labelled with the
//     active provider;
//   - every result is the structured netcheck.ToolResult — measured,
//     honest, credential-free.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Parsaetak/FreeIran/engine/netcheck"
	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// toolConcurrency bounds simultaneous tool executions (§14).
const toolConcurrency = 3

// InternetToolsService is the Wails-bound tools surface.
type InternetToolsService struct {
	app    *App
	runner *netcheck.ToolRunner
	tokens chan struct{}
}

// NewInternetToolsService creates the service with bounded
// concurrency.
func NewInternetToolsService(a *App) *InternetToolsService {
	return &InternetToolsService{
		app:    a,
		runner: netcheck.NewToolRunner(),
		tokens: make(chan struct{}, toolConcurrency),
	}
}

// Tools lists the catalogue for the UI.
func (s *InternetToolsService) Tools() []netcheck.ToolInfo {
	return netcheck.ToolCatalogue()
}

// ToolRequestView is the UI-facing request.
type ToolRequestView struct {
	// Tool is the tool id (netcheck.ToolID string).
	Tool string `json:"tool"`

	// Target is the optional user-supplied target (validated by the
	// safety layer before any bytes leave the machine).
	Target string `json:"target,omitempty"`

	// TimeoutMS bounds the run (0 = per-tool default, clamped to
	// [1s, 60s]).
	TimeoutMS int64 `json:"timeout_ms,omitempty"`

	// Tunneled runs the tool through the ACTIVE session endpoint
	// when one exists (explicit user choice — never automatic).
	Tunneled bool `json:"tunneled,omitempty"`
}

// RunTool executes ONE tool on explicit user action.
func (s *InternetToolsService) RunTool(view ToolRequestView) (netcheck.ToolResult, error) {
	tool := netcheck.ToolID(view.Tool)

	if tool == "" {
		return netcheck.ToolResult{}, fmt.Errorf("unknown tool %q", view.Tool)
	}

	// Bounded concurrency (§14): acquire a token or fail honestly
	// instead of queueing unbounded work.
	select {
	case s.tokens <- struct{}{}:
		defer func() { <-s.tokens }()
	default:
		return netcheck.ToolResult{}, errors.New("too many tools running concurrently; wait for one to finish")
	}

	request := netcheck.ToolRequest{
		Tool:    tool,
		Target:  view.Target,
		Timeout: time.Duration(view.TimeoutMS) * time.Millisecond,
	}

	// Tunnel state comes from the LIVE session only — never fabricated.
	endpoint, providerName, active := s.app.liveTunnelEndpoint()

	if view.Tunneled {
		request.Path = netcheck.PathTunneled

		if active {
			request.Provider = providerName
			request.Dial = tunnelDial(endpoint)
		}
		// No live tunnel: the runner reports the honest "no active
		// tunnel" status instead of silently probing directly.
	}

	if tool == netcheck.ToolTunnelDiagnostics && active {
		request.Tunnel = s.app.liveTunnelSnapshot(providerName, endpoint)
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 70*time.Second)
	defer cancel()

	return s.runner.Run(ctx, request), nil
}

// LiveTunnel renders the current tunnel truth for the UI (the tools
// panel header): measured, live, never fabricated.
func (s *InternetToolsService) LiveTunnel() *netcheck.TunnelSnapshot {
	endpoint, providerName, active := s.app.liveTunnelEndpoint()
	if !active {
		return &netcheck.TunnelSnapshot{Active: false}
	}

	return s.app.liveTunnelSnapshot(providerName, endpoint)
}

// liveTunnelEndpoint resolves the active session's local SOCKS
// endpoint and its provider label ("" for core-based sessions).
func (a *App) liveTunnelEndpoint() (endpoint, providerName string, active bool) {
	if a.connMgr == nil {
		return "", "", false
	}

	endpoint = a.connMgr.ActiveEndpoint()
	if endpoint == "" {
		return "", "", false
	}

	return endpoint, a.connMgr.ProviderName(), true
}

// liveTunnelSnapshot renders the measured live tunnel truth.
func (a *App) liveTunnelSnapshot(providerName, endpoint string) *netcheck.TunnelSnapshot {
	snapshot := &netcheck.TunnelSnapshot{
		Active:   true,
		Provider: providerName,
		Endpoint: endpoint,
	}

	if providerName == "" {
		// Core-based session: label it as the configuration route.
		snapshot.Provider = "configuration"
	}

	// Real measurement through the live endpoint.
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()

	dialer := socks5.Dialer{ProxyAddr: endpoint, Timeout: 5 * time.Second}

	started := time.Now()

	conn, err := dialer.Dial(ctx, "tcp", "www.gstatic.com:443")
	if err != nil {
		snapshot.Healthy = false
		snapshot.Details = "endpoint probe failed"

		return snapshot
	}

	_ = conn.Close()

	elapsed := time.Since(started)
	snapshot.Healthy = true
	snapshot.LatencyMS = elapsed.Milliseconds() // 0 = sub-ms (measured)

	return snapshot
}

// tunnelDial adapts the live endpoint to the tools' DialFunc.
func tunnelDial(endpoint string) netcheck.DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := socks5.Dialer{ProxyAddr: endpoint, Timeout: 10 * time.Second}

		return dialer.Dial(ctx, network, addr)
	}
}
