package core

import (
	"context"
	"fmt"
	"net"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// HealthReport separates process health from network health: a core
// binary can be alive while its listener is broken, and only the
// combination means the tunnel path is established.
//
// Ladder (each step implies the previous ones were observed):
//
//	process alive → core started → local listener available →
//	protocol path established → connectivity verified
//
// This report covers the first three steps; the connection manager
// optionally verifies through-proxy connectivity on top.
type HealthReport struct {
	Core          string        `json:"core"`
	State         InstanceState `json:"state"`
	ProcessAlive  bool          `json:"process_alive"`
	ListenerReady bool          `json:"listener_ready"`
	LatencyMS     int64         `json:"latency_ms"`
	CheckedAt     time.Time     `json:"checked_at"`
	Details       string        `json:"details,omitempty"`
}

// Healthy reports whether the measured axes are both good.
func (h HealthReport) Healthy() bool {
	return h.ProcessAlive && h.ListenerReady
}

// probeListener dials a local listener once and reports whether it
// accepts connections, together with the dial latency.
func probeListener(ctx context.Context, endpoint string) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, listenerProbeTimeout)
	defer cancel()

	started := time.Now()

	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return false, 0, err
	}

	_ = conn.Close()

	return true, time.Since(started), nil
}

// waitForListener polls a local endpoint until it accepts
// connections, the process dies, or the timeout elapses.
func waitForListener(
	ctx context.Context,
	endpoint string,
	timeout time.Duration,
	proc processWaiter,
) error {
	deadline := time.Now().Add(timeout)

	exited := make(chan error, 1)

	go func() { exited <- proc.Wait(ctx) }()

	for {
		ready, _, err := probeListener(ctx, endpoint)
		if ready {
			return nil
		}

		select {
		case err := <-exited:
			if err == context.Canceled || ctx.Err() != nil {
				return firerrors.Wrap(ctx.Err(), firerrors.KindRetryable,
					Subsystem, "listener", "startup cancelled")
			}

			return firerrors.New(firerrors.KindDependencyUnavailable,
				Subsystem, "listener",
				"core exited while waiting for %s: %v", endpoint, err)

		case <-ctx.Done():
			return firerrors.Wrap(ctx.Err(), firerrors.KindRetryable,
				Subsystem, "listener", "startup cancelled")

		case <-time.After(listenerPollInterval):
			if time.Now().After(deadline) {
				return firerrors.New(firerrors.KindDependencyUnavailable,
					Subsystem, "listener",
					"listener %s not ready after %s (last probe: %v)",
					endpoint, timeout, err)
			}
		}
	}
}

// processWaiter abstracts the managed-process wait for probing.
type processWaiter interface {
	Wait(ctx context.Context) error
}

const (
	// listenerProbeTimeout bounds a single listener dial.
	listenerProbeTimeout = 2 * time.Second

	// listenerPollInterval paces readiness polling.
	listenerPollInterval = 100 * time.Millisecond
)

// ReserveLocalPort allocates an ephemeral port for a local inbound.
//
// The listen-close-reuse pattern has an inherent tiny race window
// (another process could bind the port between close and the core's
// bind). The window is milliseconds on a loopback interface and the
// failure mode is a clean startup error with retry, which callers
// already handle — a deliberate trade for compatibility with cores
// that cannot report a kernel-assigned port.
//
// Backends call this BEFORE BuildConfig so the generated document
// carries the final inbound port.
func ReserveLocalPort(host string) (int, error) {
	const attempts = 16

	for attempt := 0; attempt < attempts; attempt++ {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			continue
		}

		port := listener.Addr().(*net.TCPAddr).Port

		if err := listener.Close(); err != nil {
			continue
		}

		if port > 0 {
			return port, nil
		}
	}

	return 0, fmt.Errorf("no ephemeral port available on %s after %d attempts",
		host, attempts)
}
