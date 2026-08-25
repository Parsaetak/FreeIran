package tester

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

type fakeProbe struct {
	supported map[config.Type]bool
	result    Result
	err       error
}

func (p *fakeProbe) Supports(protocol config.Type) bool {
	return p.supported[protocol]
}

func (p *fakeProbe) Test(
	_ context.Context,
	_ config.Config,
) (Result, error) {
	return p.result, p.err
}

func testConfig() config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Address:  "example.com",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "tcp",
	}
}

func TestTesterSuccessfulProbe(t *testing.T) {
	probe := &fakeProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
		result: Result{
			Working:  true,
			Latency: 42 * time.Millisecond,
		},
	}

	tester := New(probe)

	result := tester.Test(
		context.Background(),
		testConfig(),
	)

	if !result.Working {
		t.Fatalf(
			"expected configuration to work: %s",
			result.LastError,
		)
	}

	if result.Latency != 42*time.Millisecond {
		t.Fatalf(
			"expected 42ms latency, got %v",
			result.Latency,
		)
	}

	if result.TestedAt.IsZero() {
		t.Fatal("expected TestedAt to be populated")
	}
}

func TestTesterRejectsInvalidConfiguration(t *testing.T) {
	probe := &fakeProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
	}

	tester := New(probe)

	cfg := testConfig()
	cfg.UUID = ""

	result := tester.Test(
		context.Background(),
		cfg,
	)

	if result.Working {
		t.Fatal("invalid configuration was marked working")
	}

	if result.LastError == "" {
		t.Fatal("expected validation error")
	}
}

func TestTesterRejectsUnsupportedProtocol(t *testing.T) {
	probe := &fakeProbe{
		supported: map[config.Type]bool{},
	}

	tester := New(probe)

	result := tester.Test(
		context.Background(),
		testConfig(),
	)

	if result.Working {
		t.Fatal("unsupported protocol was marked working")
	}

	if result.LastError != "unsupported protocol: vless" {
		t.Fatalf(
			"unexpected error: %s",
			result.LastError,
		)
	}
}

func TestTesterPropagatesProbeFailure(t *testing.T) {
	probe := &fakeProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
		err: errors.New("connection refused"),
	}

	tester := New(probe)

	result := tester.Test(
		context.Background(),
		testConfig(),
	)

	if result.Working {
		t.Fatal("failed probe was marked working")
	}

	if result.LastError != "connection refused" {
		t.Fatalf(
			"unexpected error: %s",
			result.LastError,
		)
	}
}

func TestTesterPropagatesProbeResultError(t *testing.T) {
	probe := &fakeProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
		result: Result{
			Latency:   100 * time.Millisecond,
			LastError: "timeout",
		},
		err: errors.New("probe failed"),
	}

	tester := New(probe)

	result := tester.Test(
		context.Background(),
		testConfig(),
	)

	if result.Working {
		t.Fatal("failed probe was marked working")
	}

	if result.LastError != "timeout" {
		t.Fatalf(
			"expected probe result error, got %q",
			result.LastError,
		)
	}
}

func TestApplyResult(t *testing.T) {
	cfg := testConfig()

	testedAt := time.UnixMilli(1234567890)

	ApplyResult(&cfg, Result{
		Working:  true,
		Latency:  55 * time.Millisecond,
		TestedAt: testedAt,
	})

	if !cfg.Working {
		t.Fatal("expected configuration to be working")
	}

	if cfg.LatencyMS != 55 {
		t.Fatalf(
			"expected 55ms latency, got %d",
			cfg.LatencyMS,
		)
	}

	if cfg.TestedAt != testedAt.UnixMilli() {
		t.Fatalf(
			"unexpected TestedAt: %d",
			cfg.TestedAt,
		)
	}
}

func TestTestAndApply(t *testing.T) {
	probe := &fakeProbe{
		supported: map[config.Type]bool{
			config.TypeVLESS: true,
		},
		result: Result{
			Working:  true,
			Latency:  25 * time.Millisecond,
		},
	}

	tester := New(probe)

	cfg := testConfig()

	result := tester.TestAndApply(
		context.Background(),
		&cfg,
	)

	if !result.Working {
		t.Fatalf(
			"expected successful test: %s",
			result.LastError,
		)
	}

	if !cfg.Working {
		t.Fatal("configuration runtime state was not updated")
	}

	if cfg.LatencyMS != 25 {
		t.Fatalf(
			"expected 25ms latency, got %d",
			cfg.LatencyMS,
		)
	}

	if cfg.TestedAt == 0 {
		t.Fatal("expected TestedAt to be populated")
	}
}

func TestTestAndApplyNilConfig(t *testing.T) {
	tester := New(nil)

	result := tester.TestAndApply(
		context.Background(),
		nil,
	)

	if result.Working {
		t.Fatal("nil configuration was marked working")
	}

	if result.LastError != "configuration is nil" {
		t.Fatalf(
			"unexpected error: %s",
			result.LastError,
		)
	}
}
