package app

// NetworkService exposes the v0.9.0 Internet / Network Diagnostics
// capability to the UI (specification §3): a manual "Check Connection"
// action backed by engine/netcheck, distinguishing
//
//      1. Internet unavailable                    5. Working normally
//      2. DNS failing                             6. Proxy-only connectivity
//      3. HTTPS failing                           7. Core up, external dead
//      4. High latency
//
// Checks run asynchronously with per-probe timeouts and honour the
// application lifecycle context for cancellation. The latest report is
// cached so the UI can redisplay it without retesting.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/netcheck"
	"github.com/Parsaetak/FreeIran/internal/version"
	"github.com/Parsaetak/FreeIran/system"
)

// NetworkService is bound to the Wails runtime.
type NetworkService struct {
	app *App
}

// NewNetworkService binds a network service to the app.
func NewNetworkService(a *App) *NetworkService {
	return &NetworkService{app: a}
}

// networkReportCache stores the last report (thread-safe).
var (
	networkMu         sync.Mutex
	networkLastReport *netcheck.Report
)

// CheckConnection runs the full probe set. The proxy address is taken
// from the active connection session when one exists, so the report
// can distinguish proxy-only and core-side failures. A bounded
// context guarantees the call returns even with black-holed probes.
func (s *NetworkService) CheckConnection() (*netcheck.Report, error) {
	checker := netcheck.New(s.networkConfig())

	// Bounded wall clock: per-probe timeouts sum safely, but the
	// caller (Wails) should never wait unbounded.
	ctx, cancel := context.WithTimeout(s.app.ctx, 45*time.Second)
	defer cancel()

	report := checker.Run(ctx)

	networkMu.Lock()
	networkLastReport = &report
	networkMu.Unlock()

	s.app.logger.Info(netcheck.Subsystem, "network_check",
		"state=%s duration=%dms latency=%dms",
		report.State, report.DurationMS, report.LatencyMS)

	if report.State != netcheck.StateOK && report.State != netcheck.StateHighLatency {
		s.app.logger.Warn(netcheck.Subsystem, "network_check_issue",
			"connectivity degraded: %s (%s)", report.State, report.Summary)
	}

	return &report, nil
}

// LastReport returns the most recent report without re-running probes
// (nil when no check has run yet in this session).
func (s *NetworkService) LastReport() *netcheck.Report {
	networkMu.Lock()
	defer networkMu.Unlock()

	return networkLastReport
}

// networkConfig merges the netcheck defaults with the active session's
// local listener (enabling the proxy-path probes). v0.9.1: the
// developer override dev_net_timeout_seconds (1-120s) replaces the
// default per-probe timeout when set.
func (s *NetworkService) networkConfig() netcheck.Config {
	cfg := netcheck.Defaults()

	if override := s.app.currentSettings().DevNetTimeoutSeconds; override > 0 {
		cfg.Timeout = time.Duration(override) * time.Second
	}

	// When a session is connected, probe through its local listener
	// too — the report then distinguishes "internet blocked but proxy
	// works" from "core connected but external requests fail".
	if snap := s.app.connMgr.Snapshot(); snap.State == "connected" && snap.Endpoint != "" {
		cfg.ProxyAddr = snap.Endpoint
	}

	return cfg
}

// DiagnosticReport is the sanitized, copy/export-friendly diagnostic
// summary (specification §8): version, platform, core and storage
// state plus recent connectivity findings. It never contains
// credentials — every component is already redaction-safe.
type DiagnosticReport struct {
	GeneratedAt  string   `json:"generated_at"`
	Version      string   `json:"version"`
	Platform     string   `json:"platform"`
	AppStatus    string   `json:"app_status"`
	ConfigCount  int      `json:"config_count"`
	Cores        []string `json:"cores"`
	Storage      string   `json:"storage"`
	NetworkState string   `json:"network_state"`
	NetworkNote  string   `json:"network_note"`
	Connection   string   `json:"connection"`
	Warnings     []string `json:"warnings,omitempty"`

	// v0.9.1 developer option (dev_verbose_diagnostics): when set,
	// Technical gains the runtime detail block below (memory,
	// native acceleration, portable mode, data paths). It stays
	// redaction-safe — paths and versions only, never credentials.
	Technical []string `json:"technical,omitempty"`
}

// BuildDiagnosticReport assembles the sanitized report.
func (s *DiagnosticsService) BuildDiagnosticReport() *DiagnosticReport {
	a := s.app
	state := a.State()

	report := &DiagnosticReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Version:     version.String(),
		AppStatus:   state.Status,
		ConfigCount: state.ConfigCount,
	}

	if info := system.GetInfo(); info.OS != "" {
		report.Platform = fmt.Sprintf("%s/%s (%s)", info.OS, info.Arch, info.PlatformNote)
	}

	// Cores: one line each (name, version, state).
	if a.coreMgr != nil {
		for _, mf := range a.coreMgr.All() {
			line := fmt.Sprintf("%s: %s", mf.Name, mf.State)

			if mf.Version != "" {
				line += fmt.Sprintf(" %s", mf.Version)
			}

			if mf.State == "broken" && mf.FailureReason != "" {
				line += fmt.Sprintf(" — %s", mf.FailureReason)
			}

			report.Cores = append(report.Cores, line)
		}
	}

	// Storage summary.
	snap := state.Storage
	report.Storage = fmt.Sprintf("%d records, %d chunks, %d bytes on disk, %d dead",
		snap.Count, snap.ChunkCount, snap.DiskBytes, snap.DeadRecords)

	// Network.
	networkMu.Lock()
	last := networkLastReport
	networkMu.Unlock()

	if last != nil {
		report.NetworkState = string(last.State)
		report.NetworkNote = last.Summary
	} else {
		report.NetworkState = "not_checked"
	}

	// Connection.
	connSnap := a.connMgr.Snapshot()
	report.Connection = fmt.Sprintf("%s (core: %s, endpoint: %s)",
		connSnap.State, connSnap.Core, connSnap.Endpoint)

	// Warnings from degraded states.
	if state.Status == "degraded" {
		report.Warnings = append(report.Warnings,
			"Storage verification reported a problem — run Verify from Diagnostics → Storage.")
	}

	// v0.9.1 developer option: attach the technical runtime block.
	if a.currentSettings().DevVerboseDiagnostics {
		report.Technical = append(report.Technical, s.developerInfoLines()...)
	}

	return report
}

// FormatDiagnosticReport renders the report as readable text for the
// clipboard / export file.
func (r *DiagnosticReport) FormatDiagnosticReport() string {
	out := fmt.Sprintf("FreeIran Diagnostic Report\nVersion: %s\nPlatform: %s\nGenerated: %s\n\n",
		r.Version, r.Platform, r.GeneratedAt)

	out += fmt.Sprintf("Application status: %s\nConfigurations: %d\nStorage: %s\nConnection: %s\nNetwork: %s",
		r.AppStatus, r.ConfigCount, r.Storage, r.Connection, r.NetworkState)

	if r.NetworkNote != "" {
		out += " — " + r.NetworkNote
	}

	out += "\n\nCores:\n"

	if len(r.Cores) == 0 {
		out += "  (none discovered)\n"
	} else {
		for _, c := range r.Cores {
			out += "  " + c + "\n"
		}
	}

	if len(r.Warnings) > 0 {
		out += "\nWarnings:\n"

		for _, w := range r.Warnings {
			out += "  " + w + "\n"
		}
	}

	if len(r.Technical) > 0 {
		out += "\nTechnical details:\n"

		for _, t := range r.Technical {
			out += "  " + t + "\n"
		}
	}

	return out
}
