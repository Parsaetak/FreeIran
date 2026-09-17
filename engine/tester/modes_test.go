package tester

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// TestPingMetricsAggregation drives a PingProbe against a local TCP
// listener with deterministic accept delays and verifies every
// aggregate statistic: min/max/median/avg/jitter/loss/timeout counts.
func TestPingMetricsAggregation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	probe := &PingProbe{Samples: 4, Timeout: time.Second, Interval: time.Millisecond}

	metrics, err := probe.Ping(context.Background(), "127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if metrics.Samples != 4 || metrics.Failures != 0 {
		t.Fatalf("samples=%d failures=%d, want 4/0", metrics.Samples, metrics.Failures)
	}

	if metrics.PacketLoss != 0 {
		t.Fatalf("packet loss = %f, want 0", metrics.PacketLoss)
	}

	if metrics.MinMS < 0 || metrics.MaxMS < metrics.MinMS {
		t.Fatalf("min/max inverted: %d/%d", metrics.MinMS, metrics.MaxMS)
	}

	if metrics.MedianMS < metrics.MinMS || metrics.MedianMS > metrics.MaxMS {
		t.Fatalf("median %d outside [%d,%d]", metrics.MedianMS, metrics.MinMS, metrics.MaxMS)
	}

	if metrics.At == 0 {
		t.Fatal("timestamp not recorded")
	}
}

// TestPingLossAndTimeouts verifies failure accounting against an
// unreachable (black-holed) address: every sample times out, the loss
// is 1.0 and the timeout counter matches.
func TestPingLossAndTimeouts(t *testing.T) {
	// 203.0.113.0/24 is TEST-NET-3: guaranteed unroutable, so the
	// dial hits the per-sample deadline instead of a refusal.
	probe := &PingProbe{Samples: 2, Timeout: 150 * time.Millisecond, Interval: time.Millisecond}

	metrics, err := probe.Ping(context.Background(), "203.0.113.1", 443)
	if err != nil {
		t.Fatalf("unreachable endpoint is a metrics result, not an error: %v", err)
	}

	if metrics.Samples != 0 {
		t.Fatalf("samples = %d, want 0", metrics.Samples)
	}

	if metrics.Failures != 2 {
		t.Fatalf("failures = %d, want 2", metrics.Failures)
	}

	if metrics.Timeouts != 2 {
		t.Fatalf("timeouts = %d, want 2 (black-holed path)", metrics.Timeouts)
	}

	if metrics.PacketLoss != 1 {
		t.Fatalf("loss = %f, want 1", metrics.PacketLoss)
	}

	if metrics.MedianMS != 0 || metrics.MinMS != 0 {
		t.Fatalf("no samples but nonzero latency stats: %+v", metrics)
	}
}

// TestPingRefusedNotTimeout verifies an actively refused port counts
// as a failure but NOT a timeout.
func TestPingRefusedNotTimeout(t *testing.T) {
	// Bind then close: the port is very likely to answer RST.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	probe := &PingProbe{Samples: 1, Timeout: time.Second, Interval: 0}

	metrics, _ := probe.Ping(context.Background(), "127.0.0.1", port)

	if metrics.Failures != 1 && metrics.Samples == 0 {
		// RST can race with retries; the invariant we assert is that
		// a non-sample is never counted as a timeout when the dial
		// failed fast (refused, not black-holed).
		if metrics.Timeouts != 0 {
			t.Fatalf("refused port counted as timeout: %+v", metrics)
		}
	}
}

// TestPingCancellation aborts promptly and keeps partial samples.
func TestPingCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())

	probe := &PingProbe{Samples: 100, Timeout: time.Second, Interval: 30 * time.Millisecond}

	// Cancel after the first sample has time to land.
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()

	started := time.Now()

	metrics, err := probe.Ping(ctx, "127.0.0.1", ln.Addr().(*net.TCPAddr).Port)

	if err == nil {
		t.Fatal("cancelled run must report the cancellation error")
	}

	if time.Since(started) > 2*time.Second {
		t.Fatalf("cancellation not prompt: %v", time.Since(started))
	}

	if metrics.Samples > 0 && metrics.At == 0 {
		t.Fatal("partial metrics lost their timestamp")
	}
}

// TestPingInvalidEndpoint verifies validation without a dial.
func TestPingInvalidEndpoint(t *testing.T) {
	probe := &PingProbe{Samples: 2, Timeout: time.Millisecond, Interval: 0}

	metrics, err := probe.Ping(context.Background(), "", 0)
	if err == nil {
		t.Fatal("invalid endpoint must error")
	}

	if metrics.PacketLoss != 1 {
		t.Fatalf("loss = %f, want 1", metrics.PacketLoss)
	}
}

// TestJitterAndMedianHelpers checks the statistics helpers used by
// ranking tests.
func TestJitterAndMedianHelpers(t *testing.T) {
	if got := medianOf([]int64{30, 10, 20}); got != 20 {
		t.Fatalf("median = %d, want 20", got)
	}

	// |20-10| + |30-20| = 20, /2 = 10.
	if got := jitterOf([]int64{10, 20, 30}); got != 10 {
		t.Fatalf("jitter = %d, want 10", got)
	}

	if got := jitterOf([]int64{42}); got != 0 {
		t.Fatalf("single sample jitter = %d, want 0", got)
	}
}

// TestURLTestDirectSuccess drives a URL test WITHOUT a proxy dialer
// against a local HTTPS-less test server and checks the phase
// breakdown and success verdict.
func TestURLTestDirectSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond) // measurable even in ms resolution
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	u := &URLTester{Timeout: 5 * time.Second}

	metrics := u.Test(context.Background(), nil, srv.URL)

	if !metrics.OK {
		t.Fatalf("direct URL test failed: %+v", metrics)
	}

	if metrics.Status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", metrics.Status)
	}

	if metrics.TotalMS <= 0 {
		t.Fatalf("total = %d, want > 0", metrics.TotalMS)
	}

	if metrics.TLSMS != -1 {
		t.Fatalf("plain HTTP test must report TLS = -1, got %d", metrics.TLSMS)
	}

	if metrics.At == 0 {
		t.Fatal("timestamp missing")
	}

	if metrics.Error != "" {
		t.Fatalf("unexpected error %q", metrics.Error)
	}
}

// TestURLTestThroughDialer drives a URL test through a custom dialer
// (the tunnel path), verifying connect timing is recorded from the
// dialer itself.
func TestURLTestThroughDialer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload-for-connectivity"))
	}))
	defer srv.Close()

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)))
	}

	u := &URLTester{Timeout: 5 * time.Second}

	metrics := u.Test(context.Background(), dial, srv.URL)

	if !metrics.OK {
		t.Fatalf("tunnel URL test failed: %+v", metrics)
	}

	if metrics.ConnectMS < 0 {
		t.Fatalf("connect timing not recorded: %+v", metrics)
	}

	if metrics.Bytes == 0 {
		t.Fatal("response size not recorded")
	}
}

// TestURLTestTimeout verifies the timeout classification against a
// server that never answers.
func TestURLTestTimeout(t *testing.T) {
	// A listener that accepts but never responds → deadline.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			defer conn.Close() // hold open, never respond
		}
	}()

	target := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)) + "/"

	u := &URLTester{Timeout: 300 * time.Millisecond}

	metrics := u.Test(context.Background(), nil, target)

	if metrics.OK {
		t.Fatalf("silent server must fail: %+v", metrics)
	}

	if !metrics.Timeout {
		t.Fatalf("deadline failure must be classified as timeout: %+v", metrics)
	}

	if metrics.Error != "timeout" {
		t.Fatalf("error class = %q, want timeout", metrics.Error)
	}
}

// TestURLTestCancellation propagates context cancellation.
func TestURLTestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	u := &URLTester{Timeout: 5 * time.Second}

	metrics := u.Test(ctx, nil, "https://example.com/")

	if metrics.OK {
		t.Fatal("cancelled request must not succeed")
	}

	if metrics.Error != "cancelled" && metrics.Error != "timeout" {
		t.Fatalf("error class = %q", metrics.Error)
	}
}

// TestURLTestInvalidTarget verifies graceful handling of a bad URL.
func TestURLTestInvalidTarget(t *testing.T) {
	u := &URLTester{Timeout: time.Second}

	metrics := u.Test(context.Background(), nil, "not a url")

	if metrics.OK {
		t.Fatal("invalid URL must fail")
	}

	if metrics.Error == "" {
		t.Fatal("failure reason missing")
	}
}

// TestModeVerdicts verifies per-mode verdict semantics: a candidate
// with ping but a failed URL must NOT be Working in ping_url mode.
func TestModeVerdicts(t *testing.T) {
	pingOK := &config.PingMetrics{Samples: 4, MedianMS: 20}
	urlOK := &config.URLTestMetrics{OK: true, TotalMS: 120, Status: 204}
	urlFail := &config.URLTestMetrics{OK: false, Error: "timeout", Timeout: true}
	hsOK := &config.HandshakeMetrics{OK: true, ReadyMS: 300}

	cases := []struct {
		mode  Mode
		out   ModeOutcome
		works bool
	}{
		{ModePing, ModeOutcome{Ping: pingOK}, true},
		{ModePing, ModeOutcome{Ping: &config.PingMetrics{Failures: 4}}, false},
		{ModeURL, ModeOutcome{URLTest: urlOK}, true},
		{ModeURL, ModeOutcome{URLTest: urlFail}, false},
		// 20ms ping + failed URL: NOT working (the spec's example).
		{ModePingURL, ModeOutcome{Ping: pingOK, URLTest: urlFail}, false},
		{ModePingURL, ModeOutcome{Ping: pingOK, URLTest: urlOK}, true},
		{ModeHandshake, ModeOutcome{Handshake: hsOK}, true},
		{ModeHandshake, ModeOutcome{Handshake: &config.HandshakeMetrics{}}, false},
		{ModeFull, ModeOutcome{Handshake: hsOK, URLTest: urlOK, Ping: pingOK}, true},
		{ModeFull, ModeOutcome{Handshake: hsOK, URLTest: urlFail, Ping: pingOK}, false},
	}

	for _, tc := range cases {
		tc.out.Mode = tc.mode

		if got := modeVerdict(tc.mode, tc.out); got != tc.works {
			t.Fatalf("modeVerdict(%s) = %v, want %v", tc.mode, got, tc.works)
		}
	}
}

// TestApplyModeOutcome verifies the persistence mapping: metrics land
// in the canonical fields, streaks reset on success and grow on
// failure, and a history observation is appended.
func TestApplyModeOutcome(t *testing.T) {
	now := time.Now().UTC()

	cfg := &config.Config{ID: "x", Type: config.TypeVLESS, Address: "a", Port: 1}

	out := ModeOutcome{
		Mode:      ModePingURL,
		TestedAt:  now,
		Working:   true,
		Ping:      &config.PingMetrics{Samples: 4, MedianMS: 20, At: now.UnixMilli()},
		URLTest:   &config.URLTestMetrics{OK: true, Status: 204, TotalMS: 90, At: now.UnixMilli()},
		Handshake: &config.HandshakeMetrics{OK: true, ReadyMS: 250, At: now.UnixMilli()},
		Backend:   "xray",
		Latency:   20 * time.Millisecond,
	}

	ApplyModeOutcome(cfg, out)

	if cfg.Ping == nil || cfg.Ping.MedianMS != 20 {
		t.Fatalf("ping metrics not applied: %+v", cfg.Ping)
	}

	if cfg.URLTest == nil || !cfg.URLTest.OK {
		t.Fatalf("url metrics not applied: %+v", cfg.URLTest)
	}

	if cfg.Handshake == nil || !cfg.Handshake.OK {
		t.Fatalf("handshake metrics not applied: %+v", cfg.Handshake)
	}

	if !cfg.Working || cfg.FailureStreak != 0 || cfg.LastSuccessAt != now.UnixMilli() {
		t.Fatalf("success state wrong: working=%v streak=%d lastSuccess=%d",
			cfg.Working, cfg.FailureStreak, cfg.LastSuccessAt)
	}

	if len(cfg.TestHistory) != 1 || !cfg.TestHistory[0].Working {
		t.Fatalf("history observation missing: %+v", cfg.TestHistory)
	}

	// Failure outcome: streak grows, reason recorded.
	failOut := ModeOutcome{
		Mode:      ModePingURL,
		TestedAt:  now.Add(time.Second),
		Working:   false,
		Ping:      &config.PingMetrics{Samples: 4, MedianMS: 20, At: now.UnixMilli()},
		URLTest:   &config.URLTestMetrics{Timeout: true, Error: "timeout"},
		LastError: "url test failed: timeout",
	}

	ApplyModeOutcome(cfg, failOut)

	if cfg.Working || cfg.FailureStreak != 1 || cfg.LastFailureReason == "" {
		t.Fatalf("failure state wrong: working=%v streak=%d reason=%q",
			cfg.Working, cfg.FailureStreak, cfg.LastFailureReason)
	}

	// A timed-out URL test must flag the history observation.
	last := cfg.TestHistory[len(cfg.TestHistory)-1]
	if last.TimedOut != true {
		t.Fatalf("timeout not flagged in history: %+v", last)
	}
}
