package app

// Developer / advanced surface (v0.9.1).
//
// DeveloperInfo is a single read-only binding that answers every
// "what is the app actually doing" question the Settings → Developer
// section renders: build identity, data layout, portable mode, native
// acceleration status, live memory pressure and test-queue internals.
//
// Everything here is redaction-safe by construction: versions, paths,
// counters and booleans only — no credentials, no endpoints, no log
// content.

import (
	"fmt"
	"runtime"

	"github.com/Parsaetak/FreeIran/engine/native"
	"github.com/Parsaetak/FreeIran/internal/version"
	"github.com/Parsaetak/FreeIran/system"
)

// DeveloperInfoView is the structured developer/build information
// snapshot bound to the UI.
type DeveloperInfoView struct {
	// Build identity.
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	UserAgent string `json:"user_agent"`

	// Runtime layout.
	BaseDir      string `json:"base_dir"`
	DataDir      string `json:"data_dir"`
	LogsDir      string `json:"logs_dir"`
	CoresDir     string `json:"cores_dir"`
	RuntimeDir   string `json:"runtime_dir"`
	PortableMode bool   `json:"portable_mode"`

	// Workspace state (v0.9.2).
	WorkspaceWritable bool                   `json:"workspace_writable"`
	Migration         system.WorkspaceStatus `json:"migration"`

	// Engine status.
	NativeAcceleration   string `json:"native_acceleration"`
	QueueWorkersOverride int    `json:"queue_workers_override"`
	NetTimeoutOverride   int    `json:"net_timeout_override_seconds"`

	// Live queue internals (zero values when the queue is not up).
	QueueDepth    int   `json:"queue_depth"`
	ActiveWorkers int   `json:"active_workers"`
	TotalEnqueued int64 `json:"total_enqueued"`
	TotalPassed   int64 `json:"total_passed"`
	TotalFailed   int64 `json:"total_failed"`
}

// DeveloperInfo assembles the snapshot. Every accessor is best-effort:
// a lazy subsystem that has not started yet simply reports zeros.
func (s *DiagnosticsService) DeveloperInfo() DeveloperInfoView {
	a := s.app

	layout := a.layout

	view := DeveloperInfoView{
		Version:   version.Version,
		Commit:    version.Commit,
		GoVersion: runtime.Version(),
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		UserAgent: version.UserAgent(),

		BaseDir:      a.opts.BaseDir,
		DataDir:      layout.Data,
		LogsDir:      layout.Logs,
		CoresDir:     layout.Cores,
		RuntimeDir:   layout.Runtime,
		PortableMode: system.PortableMode(),

		Migration: system.LoadWorkspaceStatus(),

		QueueWorkersOverride: a.currentSettings().DevQueueWorkers,
		NetTimeoutOverride:   a.currentSettings().DevNetTimeoutSeconds,
	}

	if native.Available() {
		view.NativeAcceleration = "available"
	} else {
		view.NativeAcceleration = "go fallback (forced or bridge unavailable)"
	}

	if err := system.WorkspaceWritable(layout.Root); err == nil {
		view.WorkspaceWritable = true
	}

	a.initMu.Lock()
	queue := a.testQueue
	a.initMu.Unlock()

	if queue != nil {
		stats := queue.Stats()

		view.QueueDepth = stats.QueueDepth
		view.ActiveWorkers = stats.ActiveWorkers
		view.TotalEnqueued = stats.TotalEnqueued
		view.TotalPassed = stats.TotalPassed
		view.TotalFailed = stats.TotalFailed
	}

	return view
}

// developerInfoLines renders the verbose-diagnostics technical block
// (dev_verbose_diagnostics). One line per fact, plain text.
func (s *DiagnosticsService) developerInfoLines() []string {
	info := s.DeveloperInfo()

	return []string{
		fmt.Sprintf("build: %s (%s, %s)", info.Version, info.Commit, info.GoVersion),
		fmt.Sprintf("platform: %s (user agent %s)", info.Platform, info.UserAgent),
		fmt.Sprintf("portable mode: %v", info.PortableMode),
		fmt.Sprintf("workspace writable: %v", info.WorkspaceWritable),
		fmt.Sprintf("workspace migration: migrated=%v source=%q at=%q skipped=%q",
			info.Migration.Migrated, info.Migration.Source,
			info.Migration.MigratedAt, info.Migration.Skipped),
		fmt.Sprintf("base directory: %s", info.BaseDir),
		fmt.Sprintf("data directory: %s", info.DataDir),
		fmt.Sprintf("logs directory: %s", info.LogsDir),
		fmt.Sprintf("cores directory: %s", info.CoresDir),
		fmt.Sprintf("runtime directory: %s", info.RuntimeDir),
		fmt.Sprintf("native acceleration: %s", info.NativeAcceleration),
		fmt.Sprintf("queue: depth=%d active=%d enqueued=%d passed=%d failed=%d (workers override %d)",
			info.QueueDepth, info.ActiveWorkers, info.TotalEnqueued, info.TotalPassed, info.TotalFailed, info.QueueWorkersOverride),
	}
}
