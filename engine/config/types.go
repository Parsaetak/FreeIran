package config

// Network identifies the transport/network layer used by a configuration.
type Network string

const (
	NetworkTCP   Network = "tcp"
	NetworkUDP   Network = "udp"
	NetworkWebSocket Network = "ws"
	NetworkGRPC  Network = "grpc"
	NetworkHTTP  Network = "http"
	NetworkHTTP2 Network = "http2"
	NetworkQUIC  Network = "quic"
	NetworkXHTTP Network = "xhttp"
)

// Security identifies the transport security mechanism.
type Security string

const (
	SecurityNone    Security = "none"
	SecurityTLS     Security = "tls"
	SecurityReality Security = "reality"
)

// ConfigStatus represents the lifecycle state of a configuration.
type ConfigStatus string

const (
	StatusDiscovered ConfigStatus = "discovered"
	StatusValid      ConfigStatus = "valid"
	StatusTesting    ConfigStatus = "testing"
	StatusWorking    ConfigStatus = "working"
	StatusFailed     ConfigStatus = "failed"
	StatusArchived   ConfigStatus = "archived"
)

// SourceKind identifies where a configuration originated.
type SourceKind string

const (
	SourceUnknown SourceKind = "unknown"
	SourceGitHub  SourceKind = "github"
	SourceURL     SourceKind = "url"
	SourceFile    SourceKind = "file"
	SourceManual  SourceKind = "manual"
)

// TestResult represents the most recent connectivity test.
type TestResult struct {
	Status   ConfigStatus `json:"status"`
	LatencyMS int64        `json:"latency_ms,omitempty"`
	TestedAt int64        `json:"tested_at,omitempty"`
	Error    string       `json:"error,omitempty"`
}
