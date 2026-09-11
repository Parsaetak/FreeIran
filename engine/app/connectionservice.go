package app

// This file defines the connection service surface bound to the
// TypeScript frontend: connect/disconnect/reconnect, the connection
// state machine snapshot, backend registry views and the
// configuration details view (credentials redacted by default).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// ConnectionService exposes the protocol-core connection lifecycle.
type ConnectionService struct {
	app *App
}

// NewConnectionService binds a connection service to the app.
func NewConnectionService(a *App) *ConnectionService {
	return &ConnectionService{app: a}
}

// BackendView is the UI projection of one protocol-core backend
// (registry metadata; no secrets are involved).
type BackendView struct {
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	Version       string   `json:"version,omitempty"`
	Path          string   `json:"path,omitempty"`
	Priority      int      `json:"priority"`
	Summary       string   `json:"summary,omitempty"`
	Notes         []string `json:"notes,omitempty"`
	LastCheck     int64    `json:"last_check,omitempty"`
	Note          string   `json:"note,omitempty"`
	PinnedVersion string   `json:"pinned_version,omitempty"`
	Source        string   `json:"source,omitempty"`
}

// Backends lists the registered protocol-core backends with their
// availability state and verified reference versions.
func (s *ConnectionService) Backends() []BackendView {
	infos := s.app.coreRegistry.Backends()

	views := make([]BackendView, 0, len(infos))

	for _, info := range infos {
		view := BackendView{
			Name:          info.Name,
			Status:        string(info.Status),
			Version:       info.Version,
			Path:          info.Path,
			Priority:      info.Priority,
			Summary:       info.Summary,
			LastCheck:     info.LastCheck.UnixMilli(),
			Note:          info.Note,
			PinnedVersion: core.PinnedVersion(info.Name),
		}

		for _, pinned := range core.PinnedCores {
			if pinned.Name == info.Name {
				view.Source = pinned.Source

				break
			}
		}

		if info.Capabilities.Notes != nil {
			view.Notes = info.Capabilities.Notes
		}

		views = append(views, view)
	}

	return views
}

// RefreshBackends re-runs executable discovery synchronously (the
// user pressed "refresh" on the cores panel).
func (s *ConnectionService) RefreshBackends() []BackendView {
	ctx, cancel := context.WithTimeout(s.app.ctx, 30*time.Second)
	defer cancel()

	s.app.coreRegistry.Refresh(ctx)

	return s.Backends()
}

// ConnectionState returns the connection state machine snapshot.
func (s *ConnectionService) ConnectionState() connectionSnapshot {
	return s.app.connMgr.Snapshot()
}

// connectionSnapshot is the JSON-facing snapshot type: the engine
// type marshals with the exact field names the UI relies on, so the
// service returns it directly.
type connectionSnapshot = connection.Snapshot

// Connect establishes the tunnel for a stored configuration. The
// configuration is loaded by fingerprint; credentials never cross
// the service boundary in the response.
func (s *ConnectionService) Connect(configID string) (connection.Snapshot, error) {
	if configID == "" {
		return connection.Snapshot{}, fmt.Errorf("app: configuration id is required")
	}

	cfg, err := s.loadConfig(configID)
	if err != nil {
		return connection.Snapshot{}, err
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 60*time.Second)
	defer cancel()

	s.app.logger.Info("connection", "connection_start",
		"connecting configuration %s via %s", cfg.ID, cfg.DisplayURL())

	preference := s.app.currentSettings().PreferredBackend

	snapshot, err := s.app.connMgr.Connect(ctx, *cfg, core.Preferences{
		AllowFallback:    true,
		PreferredBackend: preference,
	})
	if err != nil {
		s.app.logger.Error("connection", "connection_failure", "connect", "backend",
			"connection failed (state %s): %v", snapshot.State, err)

		return snapshot, err
	}

	s.app.metricsR.AddCoreSelection()

	s.app.logger.Info("connection", "connection_success",
		"connected via %s on %s (latency %d ms)",
		snapshot.Core, snapshot.Endpoint, snapshot.LatencyMS)

	return snapshot, nil
}

// ConnectConfig establishes the tunnel for an ad-hoc configuration
// (e.g. parsed from a pasted link, not yet stored).
func (s *ConnectionService) ConnectConfig(cfg config.Config) (connection.Snapshot, error) {
	cfg.Normalize()

	ctx, cancel := context.WithTimeout(s.app.ctx, 60*time.Second)
	defer cancel()

	s.app.logger.Info("connection", "connection_start",
		"connecting ad-hoc configuration via %s", cfg.DisplayURL())

	preference := s.app.currentSettings().PreferredBackend

	snapshot, err := s.app.connMgr.Connect(ctx, cfg, core.Preferences{
		AllowFallback:    true,
		PreferredBackend: preference,
	})
	if err != nil {
		s.app.logger.Error("connection", "connection_failure", "connect", "backend",
			"ad-hoc connection failed (state %s): %v", snapshot.State, err)

		return snapshot, err
	}

	s.app.logger.Info("connection", "connection_success",
		"connected via %s on %s (latency %d ms)",
		snapshot.Core, snapshot.Endpoint, snapshot.LatencyMS)

	return snapshot, nil
}

// Disconnect tears the session down.
func (s *ConnectionService) Disconnect() connection.Snapshot {
	s.app.logger.Info("connection", "disconnect_start", "disconnecting")

	snapshot := s.app.connMgr.Disconnect()

	s.app.logger.Info("connection", "disconnect_success",
		"session stopped (final state %s)", snapshot.State)

	return snapshot
}

// Reconnect re-establishes the last session.
func (s *ConnectionService) Reconnect() (connection.Snapshot, error) {
	ctx, cancel := context.WithTimeout(s.app.ctx, 60*time.Second)
	defer cancel()

	snapshot, err := s.app.connMgr.Reconnect(ctx)

	if err != nil {
		s.app.logger.Error("connection", "connection_failure", "reconnect", "backend",
			"reconnect failed: %v", err)
	} else {
		s.app.logger.Info("connection", "connection_success",
			"reconnected via %s", snapshot.Core)
	}

	return snapshot, err
}

// Health measures the active session.
func (s *ConnectionService) Health() core.HealthReport {
	ctx, cancel := context.WithTimeout(s.app.ctx, 10*time.Second)
	defer cancel()

	return s.app.connMgr.Health(ctx)
}

// ConfigDetail is the configuration details view (§17): protocol,
// endpoint, transport, security, compatible backends, latency, last
// test, source and status. Credential fields are redacted by
// default — the raw values stay in the store, never in the view.
type ConfigDetail struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	Name         string   `json:"name,omitempty"`
	Address      string   `json:"address"`
	Port         int      `json:"port"`
	Network      string   `json:"network,omitempty"`
	Path         string   `json:"path,omitempty"`
	Host         string   `json:"host,omitempty"`
	Service      string   `json:"service,omitempty"`
	Security     string   `json:"security,omitempty"`
	ServerName   string   `json:"server_name,omitempty"`
	Method       string   `json:"method,omitempty"`
	Username     string   `json:"username,omitempty"`
	HasPassword  bool     `json:"has_password,omitempty"`
	HasUUID      bool     `json:"has_uuid,omitempty"`
	HasPublicKey bool     `json:"has_public_key,omitempty"`
	Working      bool     `json:"working"`
	LatencyMS    int64    `json:"latency_ms,omitempty"`
	TestedAt     int64    `json:"tested_at,omitempty"`
	Source       string   `json:"source,omitempty"`
	Display      string   `json:"display"`
	Backends     []string `json:"compatible_backends,omitempty"`
}

// ConfigDetails renders the details view for one stored
// configuration.
func (s *ConnectionService) ConfigDetails(configID string) (*ConfigDetail, error) {
	cfg, err := s.loadConfig(configID)
	if err != nil {
		return nil, err
	}

	detail := &ConfigDetail{
		ID:           cfg.ID,
		Type:         string(cfg.Type),
		Name:         cfg.Name,
		Address:      cfg.Address,
		Port:         cfg.Port,
		Network:      cfg.Network,
		Path:         cfg.Path,
		Host:         cfg.Host,
		Service:      cfg.Service,
		Security:     cfg.Security,
		ServerName:   cfg.ServerName,
		Method:       cfg.Method,
		Username:     cfg.Username,
		HasPassword:  cfg.Password != "",
		HasUUID:      cfg.UUID != "",
		HasPublicKey: cfg.PublicKey != "",
		Working:      cfg.Working,
		LatencyMS:    cfg.LatencyMS,
		TestedAt:     cfg.TestedAt,
		Source:       cfg.Source,
		Display:      cfg.DisplayURL(),
	}

	// Compatible backends through the registry — the same
	// deterministic resolution the connection manager uses.
	for _, info := range s.app.coreRegistry.Backends() {
		if backend, ok := s.app.coreRegistry.Get(info.Name); ok {
			if backend.Supports(*cfg) {
				detail.Backends = append(detail.Backends, info.Name)
			}
		}
	}

	return detail, nil
}

// loadConfig fetches a stored configuration by fingerprint.
func (s *ConnectionService) loadConfig(configID string) (*config.Config, error) {
	value, err := s.app.store.Get(configID)
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindInvalidInput,
			Subsystem, "connection", "load configuration %s", configID)
	}

	cfg := &config.Config{}

	if err := json.Unmarshal(value, cfg); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "connection", "decode configuration %s", configID)
	}

	cfg.ID = configID

	return cfg, nil
}
