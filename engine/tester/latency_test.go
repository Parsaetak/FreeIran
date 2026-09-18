package tester

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// latency_test.go pins the v0.9.8.1 canonical measurement
// representation (§2 of the upgrade specification) across the full
// matrix: 0 (coarse-clock artifact), 500 µs, 999 µs, 1 ms, 1.5 ms,
// 10 ms, timeout, failure and cancellation.
//
// The regression these tests guard: a real, positive, sub-millisecond
// measurement must never become ambiguous with "not measured" — the
// exact root cause of the Windows CI failure (run 35287863799,
// TestTCPProbeReachable → "latency should be measured").

func TestMeasuredLatencyMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     time.Duration
		want    time.Duration
		wantMS  int64
		quality string
	}{
		{
			// A successful dial that completed within one coarse
			// clock tick (Windows CI runners): the raw reading is 0
			// but the success proves time passed.
			name:    "coarse clock zero",
			raw:     0,
			want:    ClockFloor,
			wantMS:  0,
			quality: QualityExcellent,
		},
		{
			name:    "negative artifact clamped",
			raw:     -time.Millisecond,
			want:    ClockFloor,
			wantMS:  0,
			quality: QualityExcellent,
		},
		{
			name:    "500 microseconds",
			raw:     500 * time.Microsecond,
			want:    500 * time.Microsecond,
			wantMS:  0,
			quality: QualityExcellent,
		},
		{
			name:    "999 microseconds",
			raw:     999 * time.Microsecond,
			want:    999 * time.Microsecond,
			wantMS:  0,
			quality: QualityExcellent,
		},
		{
			name:    "1 millisecond",
			raw:     time.Millisecond,
			want:    time.Millisecond,
			wantMS:  1,
			quality: QualityExcellent,
		},
		{
			name:    "1.5 milliseconds",
			raw:     1500 * time.Microsecond,
			want:    1500 * time.Microsecond,
			wantMS:  1,
			quality: QualityExcellent,
		},
		{
			name:    "10 milliseconds",
			raw:     10 * time.Millisecond,
			want:    10 * time.Millisecond,
			wantMS:  10,
			quality: QualityExcellent,
		},
		{
			name:    "600 milliseconds (acceptable band)",
			raw:     600 * time.Millisecond,
			want:    600 * time.Millisecond,
			wantMS:  600,
			quality: QualityAcceptable,
		},
		{
			name:    "3 seconds (very slow band)",
			raw:     3 * time.Second,
			want:    3 * time.Second,
			wantMS:  3000,
			quality: QualityVerySlow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := MeasuredLatency(tc.raw)

			if got != tc.want {
				t.Fatalf("MeasuredLatency(%v) = %v, want %v", tc.raw, got, tc.want)
			}

			if got <= 0 {
				t.Fatalf("measured latency must be strictly positive, got %v", got)
			}

			if ms := MSOf(got); ms != tc.wantMS {
				t.Fatalf("MSOf(%v) = %d, want %d", got, ms, tc.wantMS)
			}

			if q := QualityForDuration(got); q != tc.quality {
				t.Fatalf("QualityForDuration(%v) = %q, want %q", got, q, tc.quality)
			}

			// A measured 0 ms projection must classify as excellent,
			// never as failed — the pre-fix bug.
			if ms := MSOf(got); ms == 0 {
				if q := QualityForMeasured(ms, true); q != QualityExcellent {
					t.Fatalf("measured sub-ms classified %q, want excellent", q)
				}
			}
		})
	}
}

func TestQualityForMeasuredSemantics(t *testing.T) {
	t.Parallel()

	// Unmeasured or invalid input is "failed".
	for _, tc := range []struct {
		ms       int64
		measured bool
	}{
		{0, false}, {1, false}, {-1, true}, {-5, false},
	} {
		if q := QualityForMeasured(tc.ms, tc.measured); q != QualityFailed {
			t.Fatalf("QualityForMeasured(%d, %v) = %q, want failed",
				tc.ms, tc.measured, q)
		}
	}

	// Measured sub-ms is the excellent band.
	if q := QualityForMeasured(0, true); q != QualityExcellent {
		t.Fatalf("QualityForMeasured(0, true) = %q, want excellent", q)
	}
}

func TestMSOfQuantized(t *testing.T) {
	t.Parallel()

	// Sub-millisecond measurements quantize to 1 ms for coarse
	// aggregates whose 0 is a "no data" sentinel.
	for _, raw := range []time.Duration{ClockFloor, 500 * time.Microsecond, 999 * time.Microsecond} {
		if ms := MSOfQuantized(raw); ms != 1 {
			t.Fatalf("MSOfQuantized(%v) = %d, want 1", raw, ms)
		}
	}

	if ms := MSOfQuantized(1500 * time.Microsecond); ms != 1 {
		t.Fatalf("MSOfQuantized(1.5ms) = %d, want 1", ms)
	}

	if ms := MSOfQuantized(10 * time.Millisecond); ms != 10 {
		t.Fatalf("MSOfQuantized(10ms) = %d, want 10", ms)
	}
}

func TestLatencyText(t *testing.T) {
	t.Parallel()

	if got := LatencyText(82, true); got != "82 ms" {
		t.Fatalf("LatencyText(82, true) = %q", got)
	}

	if got := LatencyText(0, true); got != "< 1 ms" {
		t.Fatalf("LatencyText(0, true) = %q, want \"< 1 ms\"", got)
	}

	if got := LatencyText(0, false); got != "—" {
		t.Fatalf("LatencyText(0, false) = %q, want em dash", got)
	}
}

// TestTCPProbeReachableRepresentation verifies the live probe's
// representation contract on a real loopback listener: a successful
// dial yields Measured=true with a strictly positive Latency, and the
// millisecond projections follow rules R3/R4 even when the dial is
// sub-millisecond (which a loopback listener usually is).
func TestTCPProbeReachableRepresentation(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}

	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	probe := NewTCPProbe()
	cfg := config.Config{
		Type:    config.TypeVLESS,
		Address: "127.0.0.1",
		Port:    listener.Addr().(*net.TCPAddr).Port,
	}

	result, err := probe.Test(context.Background(), cfg)
	if err != nil {
		t.Fatalf("test: %v", err)
	}

	if !result.Working {
		t.Fatalf("endpoint should be reachable: %s", result.LastError)
	}

	// The Windows CI regression: latency must be a strictly positive
	// measured duration — never an ambiguous zero.
	if !result.Measured {
		t.Fatal("successful probe must be Measured=true")
	}

	if result.Latency <= 0 {
		t.Fatalf("measured latency must be strictly positive, got %v", result.Latency)
	}

	if result.Quality == QualityFailed {
		t.Fatalf("a successful probe must never classify as failed (quality=%q, latency=%v)",
			result.Quality, result.Latency)
	}

	if result.PingMS < 0 || result.DurationMS < 0 {
		t.Fatalf("millisecond projections must be non-negative: ping=%d duration=%d",
			result.PingMS, result.DurationMS)
	}

	// Sub-millisecond success: projections may legitimately read 0,
	// but only alongside Measured=true (verified above).
	if result.PingMS == 0 && result.Latency >= time.Millisecond {
		t.Fatalf("PingMS=0 requires a sub-millisecond latency, got %v", result.Latency)
	}
}

// TestTCPProbeUnreachableNotMeasured verifies the failure path: no
// measurement is claimed for a failed dial.
func TestTCPProbeUnreachableNotMeasured(t *testing.T) {
	t.Parallel()

	probe := NewTCPProbe()
	cfg := config.Config{
		Type:    config.TypeTrojan,
		Address: "127.0.0.1",
		Port:    1, // nothing listens here in test environments
	}

	result, err := probe.Test(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unreachable must be a Result, not an error: %v", err)
	}

	if result.Working {
		t.Fatal("port 1 must not be reachable")
	}

	if result.Measured {
		t.Fatal("a failed dial carries no latency measurement")
	}

	if result.Latency != 0 {
		t.Fatalf("failed dial latency must be zero, got %v", result.Latency)
	}

	if result.Quality != QualityFailed {
		t.Fatalf("failed dial quality = %q, want failed", result.Quality)
	}
}

// TestTCPProbeCancelled verifies cancellation propagation: a
// cancelled context aborts the dial and produces a failed (not
// measured) result without hanging.
func TestTCPProbeCancelled(t *testing.T) {
	t.Parallel()

	probe := TCPProbe{Timeout: 5 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the dial starts

	cfg := config.Config{
		Type:    config.TypeVLESS,
		Address: "10.255.255.1", // unroutable in test environments
		Port:    65534,
	}

	result, err := probe.Test(ctx, cfg)
	if err != nil {
		t.Fatalf("cancelled probe must be a Result, not an error: %v", err)
	}

	if result.Working {
		t.Fatal("cancelled probe must not report working")
	}

	if result.Measured {
		t.Fatal("cancelled probe carries no measurement")
	}
}

// TestTCPProbeTimeoutRepresentation verifies the timeout path keeps
// its measurement-free failure semantics while still reporting the
// attempt's wall time as operational telemetry.
func TestTCPProbeTimeoutRepresentation(t *testing.T) {
	t.Parallel()

	probe := TCPProbe{Timeout: 250 * time.Millisecond}

	cfg := config.Config{
		Type:    config.TypeVLESS,
		Address: "10.255.255.1", // unroutable in test environments
		Port:    65534,
	}

	started := time.Now()

	result, err := probe.Test(context.Background(), cfg)
	if err != nil {
		t.Fatalf("timeout must be a Result, not an error: %v", err)
	}

	elapsed := time.Since(started)

	if result.Working || result.Measured {
		t.Fatal("timed-out probe must be failed and unmeasured")
	}

	if result.DurationMS <= 0 && elapsed >= time.Millisecond {
		t.Fatalf("timeout wall time should be reported, duration=%d elapsed=%v",
			result.DurationMS, elapsed)
	}
}

// TestPingSubMillisecondSamples verifies the repeated-sample ping
// path: sub-millisecond loopback samples produce a measured,
// honest PingMetrics — Samples > 0, SubMS flagged, zero projected
// median, zero invented values.
func TestPingSubMillisecondSamples(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}

	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	probe := &PingProbe{Samples: 3, Timeout: 2 * time.Second, Interval: 0}

	metrics, pingErr := probe.Ping(context.Background(), "127.0.0.1",
		listener.Addr().(*net.TCPAddr).Port)
	if pingErr != nil {
		t.Fatalf("ping: %v", pingErr)
	}

	if metrics.Samples != 3 {
		t.Fatalf("samples = %d, want 3 (failures=%d)", metrics.Samples, metrics.Failures)
	}

	if metrics.PacketLoss != 0 {
		t.Fatalf("packet loss = %f, want 0", metrics.PacketLoss)
	}

	if metrics.MedianMS < 0 || metrics.MinMS < 0 || metrics.MaxMS < 0 {
		t.Fatalf("projected stats must be non-negative: %+v", metrics)
	}

	// A loopback ping is normally sub-millisecond: the projected
	// median is 0 with SubMS=true — a REAL measurement, never
	// "unmeasured" (Samples > 0 is the measured signal).
	if metrics.MedianMS == 0 && !metrics.SubMS {
		t.Fatal("sub-millisecond median must set SubMS=true")
	}

	if metrics.MedianMS > 0 && metrics.SubMS {
		t.Fatal("SubMS must not be set for a >= 1 ms median")
	}
}

// TestPingInvalidEndpointHonestCounts strengthens the existing
// modes_test coverage: invalid input produces honest 100%-loss
// metrics and an error, never fabricated latencies.
func TestPingInvalidEndpointHonestCounts(t *testing.T) {
	t.Parallel()

	probe := &PingProbe{Samples: 2, Timeout: 500 * time.Millisecond}

	metrics, err := probe.Ping(context.Background(), "", 0)
	if err == nil {
		t.Fatal("invalid endpoint must return an error")
	}

	if metrics.Samples != 0 || metrics.Failures != 2 {
		t.Fatalf("invalid endpoint metrics: %+v", metrics)
	}

	if metrics.PacketLoss != 1 {
		t.Fatalf("packet loss = %f, want 1", metrics.PacketLoss)
	}
}

// TestPingCancelled keeps partial data: a cancelled run returns the
// samples completed before cancellation (honest bookkeeping), wrapped
// with the cancellation error.
func TestPingCancelled(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}

	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())

	// Space samples out so the cancellation lands mid-run: the first
	// loopback sample completes immediately, the second is due after
	// the interval, and the cancel fires inside that gap.
	probe := &PingProbe{Samples: 4, Timeout: 2 * time.Second, Interval: 400 * time.Millisecond}

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	metrics, err := probe.Ping(ctx, "127.0.0.1", listener.Addr().(*net.TCPAddr).Port)
	if err == nil {
		t.Fatal("cancelled ping must return the cancellation error")
	}

	if metrics.Samples == 0 {
		t.Fatal("partial samples completed before cancellation must be kept")
	}

	if metrics.Samples > 4 {
		t.Fatalf("samples = %d exceeds configured 4", metrics.Samples)
	}
}
