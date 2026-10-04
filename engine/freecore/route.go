package freecore

import (
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// OutboundKind selects the remote path of a route.
type OutboundKind string

const (
	// OutboundSOCKS5 forwards through a SOCKS5 remote.
	OutboundSOCKS5 OutboundKind = "socks5"

	// OutboundHTTP forwards through an HTTP CONNECT remote.
	OutboundHTTP OutboundKind = "http"

	// OutboundShadowsocks forwards through a Shadowsocks AEAD remote
	// (the first encrypted protocol slice — evidence-gated, see
	// docs/protocols.md).
	OutboundShadowsocks OutboundKind = "shadowsocks"

	// OutboundDirect dials the destination with no remote (the
	// engine-side direct path; not selectable as a configuration
	// type today — it exists because the outbound matrix needs it
	// and Phase 2 routing grows into it).
	OutboundDirect OutboundKind = "direct"
)

// Route is the ONE normalized engine model of a configuration: the
// destination remote, its credentials, the transport shape and the
// hop timing bounds. It is produced exactly once from the universal
// config.Config (Normalize) and every engine subsystem consumes this
// type — protocol implementations never re-parse the application
// configuration.
type Route struct {
	// ConfigID and Fingerprint carry the configuration identity for
	// logs and session metadata (credential-free).
	ConfigID     string
	Fingerprint  string
	DisplayURL   string
	Outbound     OutboundKind
	Endpoint     Endpoint
	Username     string
	Password     string
	Method       string // AEAD method name for Shadowsocks ("" otherwise)
	Network      Network
	Security     TransportSecurity
	DialTimeout  time.Duration
	AuthRequired bool
}

// Normalize converts the universal application configuration into the
// engine's internal Route model — the single normalization point. It
// refuses every shape the first-party engine does not genuinely
// implement; the caller (registry selection) routes refused shapes to
// the external compatibility cores instead.
func Normalize(cfg config.Config) (Route, error) {
	cfg.Normalize()

	route := Route{
		ConfigID:    cfg.ID,
		Fingerprint: cfg.Fingerprint(),
		DisplayURL:  cfg.DisplayURL(),
		Network:     NetworkTCP,
		Security:    SecurityNone,
	}

	switch cfg.Type {
	case config.TypeSOCKS:
		route.Outbound = OutboundSOCKS5
	case config.TypeHTTP:
		route.Outbound = OutboundHTTP
	case config.TypeShadowsocks:
		method, merr := NormalizeShadowsocksMethod(cfg.Method)
		if merr != nil {
			return Route{}, merr
		}

		route.Outbound = OutboundShadowsocks
		route.Method = method
	default:
		return Route{}, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "normalize",
			"first-party engine does not implement protocol %q (external-core fallback)", cfg.Type)
	}

	// Transport: plain TCP only. The capability matcher already
	// enforces this; normalization repeats the gate so a future
	// caller cannot bypass Supports and smuggle a WS/gRPC shape in.
	if network := strings.TrimSpace(cfg.Network); network != "" &&
		network != string(NetworkTCP) {
		return Route{}, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "normalize",
			"first-party engine implements plain TCP only (got network %q)", cfg.Network)
	}

	// Security: none only — same reasoning.
	if security := strings.TrimSpace(cfg.Security); security != "" &&
		security != string(SecurityNone) {
		return Route{}, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "normalize",
			"first-party engine implements no transport security layer (got %q)", cfg.Security)
	}

	// Proxy chains are the single-core compilation model today;
	// in-engine detours are Phase 2 item 13.
	if cfg.IsChain() {
		return Route{}, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "normalize",
			"first-party engine does not implement in-engine chains (Phase 2)")
	}

	if cfg.Type == config.TypeShadowsocks && strings.TrimSpace(cfg.Password) == "" {
		return Route{}, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "normalize", "shadowsocks remote requires a password")
	}

	if strings.TrimSpace(cfg.Address) == "" {
		return Route{}, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "normalize", "remote address is empty")
	}

	if cfg.Port <= 0 || cfg.Port > 65535 {
		return Route{}, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "normalize", "remote port %d out of range", cfg.Port)
	}

	route.Endpoint = Endpoint{Host: strings.TrimSpace(cfg.Address), Port: cfg.Port}
	route.Username = cfg.Username
	route.Password = cfg.Password
	route.AuthRequired = cfg.Username != "" || cfg.Password != ""
	route.DialTimeout = DefaultDialTimeout

	return route, nil
}
