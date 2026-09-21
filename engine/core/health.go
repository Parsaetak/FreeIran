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

// readinessOutcome is the single authoritative verdict of the startup
// supervision path (v0.9.9): one observer, one verdict, published once.
type readinessOutcome int

const (
	// readinessReady: the listener accepted a connection.
	readinessReady readinessOutcome = iota

	// readinessProcessExited: the supervised process died before the
	// listener became ready.
	readinessProcessExited

	// readinessTimedOut: the hard startup deadline elapsed.
	readinessTimedOut

	// readinessCancelled: the launch context was cancelled.
	readinessCancelled
)

// readinessProbeDelays is the adaptive readiness schedule (v0.9.9):
// probe immediately at spawn, then back off — short delays first, a
// slightly larger delay each step, bounded at readinessMaxInterval.
// A local listener that is going to open usually opens within the
// first few milliseconds; the long tail is covered by the bounded
// cadence and the hard deadline. No busy-waiting, no per-iteration
// timer allocations (one reusable timer, reset per step).
var readinessProbeDelays = []time.Duration{
	0,
	2 * time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
}

// readinessMaxInterval is the bounded polling cadence reached after
// the adaptive ramp.
const readinessMaxInterval = 100 * time.Millisecond

// awaitListener is THE startup supervision path (v0.9.9): it probes
// the local listener on an adaptive schedule, watches the process for
// early death through ONE process-wait observer, and returns a single
// verdict at the hard deadline. Callers publish the verdict exactly
// once (Instance.markReady) and own the deterministic teardown for
// the timeout outcome.
//
// Contract preserved from the pre-0.9.9 watcher: process-death
// detection, startup timeout, cancellation, no busy-waiting. Changed:
// the duplicated per-consumer observers and the fixed 100 ms
// time.After polling loop are gone.
func awaitListener(
	ctx context.Context,
	endpoint string,
	timeout time.Duration,
	proc processWaiter,
) (readinessOutcome, error) {
	deadline := time.Now().Add(timeout)

	exited := make(chan error, 1)

	go func() { exited <- proc.Wait(ctx) }()

	timer := time.NewTimer(0) // fires immediately: first probe is free
	defer timer.Stop()

	nextDelay := func(attempt int) time.Duration {
		if attempt < len(readinessProbeDelays) {
			return readinessProbeDelays[attempt]
		}

		return readinessMaxInterval
	}

	for attempt := 0; ; attempt++ {
		// Probe (the first probe runs immediately, before any wait).
		ready, _, err := probeListener(ctx, endpoint)
		if ready {
			return readinessReady, nil
		}

		// Wait for the adaptive delay — or an earlier verdict.
		timer.Reset(nextDelay(attempt))

		select {
		case err := <-exited:
			if err == context.Canceled || ctx.Err() != nil {
				return readinessCancelled, firerrors.Wrap(ctx.Err(),
					firerrors.KindRetryable, Subsystem, "listener",
					"startup cancelled")
			}

			return readinessProcessExited, firerrors.New(
				firerrors.KindDependencyUnavailable, Subsystem, "listener",
				"core exited while waiting for %s: %v", endpoint, err)

		case <-ctx.Done():
			return readinessCancelled, firerrors.Wrap(ctx.Err(),
				firerrors.KindRetryable, Subsystem, "listener",
				"startup cancelled")

		case <-timer.C:
			if time.Now().After(deadline) {
				return readinessTimedOut, firerrors.New(
					firerrors.KindDependencyUnavailable, Subsystem, "listener",
					"listener %s not ready after %s (last probe: %v)",
					endpoint, timeout, err)
			}
		}
	}
}

// ResolveInboundPort is the ONE authoritative execution-stage port
// resolver (v0.9.9). Call it BEFORE configuration generation so the
// generated runtime document embeds the final inbound port:
//
//   - requested > 0: an explicit (user-selected or test-pinned) port
//     passes through unchanged; conflict detection stays at the two
//     existing authorities (the caller's bindability pre-check and the
//     core's own bind);
//   - requested == 0: exactly ONE ephemeral allocation happens here.
//
// The pre-0.9.9 adapters each carried their own ReserveLocalPort call
// AND the shared launcher had a second one — duplicated allocation
// logic that could drift. Adapters now resolve through this function
// and Launch requires an already-resolved port.
func ResolveInboundPort(host string, requested int) (int, error) {
	if requested > 0 {
		return requested, nil
	}

	port, err := ReserveLocalPort(host)
	if err != nil {
		return 0, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "port", "reserve local port")
	}

	return port, nil
}

// processWaiter abstracts the managed-process wait for probing.
type processWaiter interface {
	Wait(ctx context.Context) error
}

const (
	// listenerProbeTimeout bounds a single listener dial.
	listenerProbeTimeout = 2 * time.Second
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
