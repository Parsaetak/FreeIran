package tester

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// ping.go implements the Ping test mode (v0.9.6 §8): repeated-sample
// latency measurement against a candidate's remote endpoint.
//
// Why TCP and not ICMP: ICMP echo requires raw sockets (root/Admin-
// istrator) or privileged helpers on every supported platform, and is
// also what censors filter first. The TCP handshake round-trip to the
// candidate's own endpoint port measures the same path (SYN → SYN/ACK
// RTT) that the production protocol will use, needs no privileges,
// and works for every protocol family. This is the standard user-space
// ping technique (hysteria/sing-box "tcping", Cloudflare's SYN-based
// RTT). The measurement is labelled a TCP ping in the UI precisely so
// nothing is claimed about ICMP.
//
// Statistics are computed ONLY from recorded samples; a cancelled run
// returns the partial metrics alongside the context error so no
// measured data is thrown away.

// PingProbe measures endpoint latency over repeated TCP samples.
type PingProbe struct {
	// Samples is the number of round trips to attempt
	// (default DefaultPingSamples).
	Samples int

	// Timeout bounds one sample's dial (default DefaultPingTimeout).
	Timeout time.Duration

	// Interval spaces consecutive samples so bursts do not look like
	// self-inflicted congestion (default DefaultPingInterval).
	Interval time.Duration
}

// DefaultPingSamples keeps a ping test lightweight: four samples give
// a median and a jitter estimate in well under a second on a healthy
// path while avoiding the "one lucky SYN" false positive.
const DefaultPingSamples = 4

// DefaultPingTimeout bounds one sample. It is deliberately generous:
// a high-latency path (>1s) is still usable through a tunnel, and the
// timeout classification (path black-holing) is more valuable than a
// fast failure.
const DefaultPingTimeout = 3 * time.Second

// DefaultPingInterval avoids back-to-back SYNs distorting jitter.
const DefaultPingInterval = 120 * time.Millisecond

// ErrPingCancelled is returned (wrapped with the context error) when
// a ping run is interrupted; the returned metrics contain every
// sample completed before cancellation.
var ErrPingCancelled = errors.New("tester: ping cancelled")

// normalize fills defaults.
func (p *PingProbe) normalize() *PingProbe {
	if p == nil {
		p = &PingProbe{}
	}

	if p.Samples <= 0 {
		p.Samples = DefaultPingSamples
	}

	if p.Timeout <= 0 {
		p.Timeout = DefaultPingTimeout
	}

	if p.Interval < 0 {
		p.Interval = DefaultPingInterval
	}

	return p
}

// Ping measures the endpoint address:port over repeated TCP samples
// and returns the full metrics set. The error is non-nil ONLY for
// cancellation or an unresolvable address; failed samples are part of
// the metrics (packet loss), not an error.
func (p *PingProbe) Ping(ctx context.Context, address string, port int) (config.PingMetrics, error) {
	p = p.normalize()

	metrics := config.PingMetrics{At: time.Now().UTC().UnixMilli()}

	if address == "" || port <= 0 || port > 65535 {
		metrics.Failures = p.Samples
		metrics.PacketLoss = 1

		return metrics, fmt.Errorf("tester: invalid ping endpoint %s:%d", address, port)
	}

	target := net.JoinHostPort(address, strconv.Itoa(port))

	rtts := make([]int64, 0, p.Samples)

	for i := 0; i < p.Samples; i++ {
		if i > 0 && p.Interval > 0 {
			select {
			case <-ctx.Done():
				return finishPing(metrics, rtts), fmt.Errorf("%w: %w", ErrPingCancelled, ctx.Err())
			case <-time.After(p.Interval):
			}
		}

		if err := ctx.Err(); err != nil {
			return finishPing(metrics, rtts), fmt.Errorf("%w: %w", ErrPingCancelled, err)
		}

		sampleCtx, cancel := context.WithTimeout(ctx, p.Timeout)

		started := time.Now()

		d := net.Dialer{}

		conn, err := d.DialContext(sampleCtx, "tcp", target)

		elapsed := time.Since(started)

		cancel()

		if conn != nil {
			_ = conn.Close()
		}

		if err == nil {
			rtts = append(rtts, elapsed.Milliseconds())

			continue
		}

		metrics.Failures++

		if isDeadlineErr(err) {
			metrics.Timeouts++
		}

		// A cancelled parent counts as a cancelled run, not a failed
		// sample; keep the honest counts and stop sampling.
		if ctx.Err() != nil {
			return finishPing(metrics, rtts), fmt.Errorf("%w: %w", ErrPingCancelled, ctx.Err())
		}
	}

	return finishPing(metrics, rtts), nil
}

// finishPing derives the aggregate statistics from the successful
// samples. With zero samples the metrics honestly report 100% loss.
func finishPing(m config.PingMetrics, rtts []int64) config.PingMetrics {
	m.Samples = len(rtts)

	attempted := m.Samples + m.Failures

	if attempted > 0 {
		m.PacketLoss = float64(m.Failures) / float64(attempted)
	}

	if len(rtts) == 0 {
		return m
	}

	sorted := append([]int64(nil), rtts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	m.MinMS = sorted[0]
	m.MaxMS = sorted[len(sorted)-1]
	m.MedianMS = sorted[len(sorted)/2]

	var sum int64

	for _, v := range rtts {
		sum += v
	}

	m.AvgMS = sum / int64(len(rtts))

	// Jitter: mean absolute deviation between consecutive samples.
	// A single sample has no deviation — jitter stays 0, which is
	// honest ("not enough samples to estimate"), never invented.
	if len(rtts) > 1 {
		var devSum int64

		for i := 1; i < len(rtts); i++ {
			d := rtts[i] - rtts[i-1]

			if d < 0 {
				d = -d
			}

			devSum += d
		}

		m.JitterMS = devSum / int64(len(rtts)-1)
	}

	return m
}

// isDeadlineErr reports whether the dial failed because of the
// deadline (path black-holing) rather than an active refusal.
func isDeadlineErr(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var ne net.Error

	return errors.As(err, &ne) && ne.Timeout()
}

// jitterOf is exposed for the ranking layer's unit tests: the mean
// absolute deviation of consecutive values. Values need not be sorted
// (jitter is an ordering statistic by definition).
func jitterOf(values []int64) int64 {
	if len(values) < 2 {
		return 0
	}

	var devSum int64

	for i := 1; i < len(values); i++ {
		d := values[i] - values[i-1]

		if d < 0 {
			d = -d
		}

		devSum += d
	}

	return devSum / int64(len(values)-1)
}

// medianOf is exposed for the ranking layer's unit tests.
func medianOf(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}

	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	return sorted[len(sorted)/2]
}

var _ = math.Abs // keep math imported for future statistical helpers
