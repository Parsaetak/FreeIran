package tester

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// TCPProbe performs endpoint reachability testing for any protocol.
//
// It is the engine's default probe: a configuration is not promoted
// until its endpoint at least accepts a TCP connection. Protocol-aware
// probes (Xray, sing-box, WireGuard cores) implement the same Probe
// interface and are registered on top; the TCPProbe remains the
// fallback that always supports every protocol.
type TCPProbe struct {
	// Timeout bounds one dial attempt.
	Timeout time.Duration
}

// DefaultTCPTimeout bounds reachability attempts.
const DefaultTCPTimeout = 5 * time.Second

// NewTCPProbe creates a probe with safe defaults.
func NewTCPProbe() *TCPProbe {
	return &TCPProbe{Timeout: DefaultTCPTimeout}
}

// Supports reports that every protocol can be reachability-tested.
func (p *TCPProbe) Supports(config.Type) bool {
	return true
}

// Test dials the configuration endpoint and measures latency.
func (p *TCPProbe) Test(ctx context.Context, cfg config.Config) (Result, error) {
	timeout := p.Timeout

	if timeout <= 0 {
		timeout = DefaultTCPTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()

	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp",
		net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port)))
	if err != nil {
		// A failed dial carries no latency measurement (Measured
		// stays false); DurationMS reports the failed attempt's wall
		// time as operational telemetry.
		return Result{
			Working:    false,
			TestedAt:   time.Now().UTC(),
			LastError:  "endpoint unreachable: " + err.Error(),
			Backend:    "tcp",
			Protocol:   string(cfg.Type),
			Endpoint:   net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port)),
			DurationMS: time.Since(started).Milliseconds(),
			Quality:    QualityFailed,
		}, nil
	}

	_ = conn.Close()

	// v0.9.8.1 (§2): MeasuredLatency guarantees a strictly positive
	// duration for this successful dial. On platforms with coarse
	// monotonic clocks (Windows CI runners) a fast loopback connect
	// can complete within one clock tick, making the raw reading
	// exactly 0 — a successful dial proves time passed, so the reading
	// is quantized up to ClockFloor instead of being stored as an
	// ambiguous zero. No artificial sleep is inserted.
	latency := MeasuredLatency(time.Since(started))

	return Result{
		Working:    true,
		Latency:    latency,
		Measured:   true,
		TestedAt:   time.Now().UTC(),
		Backend:    "tcp",
		Protocol:   string(cfg.Type),
		Endpoint:   net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port)),
		PingMS:     MSOf(latency),
		DurationMS: MSOf(latency),
		Quality:    QualityForDuration(latency),
	}, nil
}
