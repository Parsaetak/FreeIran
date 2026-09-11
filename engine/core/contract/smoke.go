package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
)

// SmokeOptions describe one real-binary smoke test.
type SmokeOptions struct {
	// Backend under test.
	Backend core.Core

	// BinaryPath is the real protocol-core executable.
	BinaryPath string

	// Configs are validated and started against the real binary.
	Configs []config.Config

	// ValidateArgs renders the config-validation command for the
	// core family (e.g. ["test", "-c", file] for v2ray,
	// ["run", "-test", "-c", file] for xray,
	// ["check", "-c", file] for sing-box).
	ValidateArgs func(file string) []string

	// Timeout bounds each validation/startup.
	Timeout time.Duration
}

// RunSmoke validates generated configurations against a REAL
// protocol-core binary: first through the core's own config test
// command, then through a full startup + listener-ready + shutdown
// cycle.
//
// Smoke tests are the acceptance layer for the capability
// declarations: the deterministic adapter tests prove structure, the
// smoke tests prove the real core accepts and runs the document.
func RunSmoke(t *testing.T, opts SmokeOptions) {
	t.Helper()

	if opts.BinaryPath == "" {
		t.Skip("no real core binary supplied (set the FREEIRAN_TEST_*_BIN environment variable)")
	}

	timeout := opts.Timeout

	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	workDir := t.TempDir()

	for _, cfg := range opts.Configs {
		cfg := cfg

		name := fmt.Sprintf("%s_%s_%s", cfg.Type, cfg.Network, cfg.Security)

		if name == "" || name == "__" {
			name = string(cfg.Type)
		}

		t.Run(name, func(t *testing.T) {
			smokeOne(t, opts, cfg, workDir, timeout)
		})
	}
}

// smokeOne runs one configuration through validation and startup.
func smokeOne(t *testing.T, opts SmokeOptions, cfg config.Config, workDir string, timeout time.Duration) {
	t.Helper()

	port := reservePort(t)

	doc, err := opts.Backend.BuildConfig(cfg, core.RuntimeOptions{
		LocalPort:       port,
		BinaryPath:      opts.BinaryPath,
		DisableGenCache: true,
	})
	if err != nil {
		t.Fatalf("BuildConfig() = %v", err)
	}

	file := filepath.Join(workDir, doc.FileName)

	if err := os.WriteFile(file, doc.Data, 0o600); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}

	t.Cleanup(func() { _ = os.Remove(file) })

	// Stage 1: the real core validates its own configuration.
	validate := exec.Command(opts.BinaryPath, opts.ValidateArgs(file)...)

	if out, err := validate.CombinedOutput(); err != nil {
		t.Fatalf("real core rejected the generated configuration: %v\n%s",
			err, redactOutput(string(out), cfg))
	}

	// Stage 2: full startup through the shared launcher.
	instance, err := opts.Backend.Start(context.Background(), cfg, core.RuntimeOptions{
		LocalPort:       port,
		BinaryPath:      opts.BinaryPath,
		StartupTimeout:  timeout,
		DisableGenCache: true,
		WorkDir:         workDir,
	})
	if err != nil {
		t.Fatalf("Start() = %v", err)
	}

	t.Cleanup(func() { _ = instance.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := instance.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady() = %v (logs: %s)", err, instance.Logs())
	}

	report := instance.Health(ctx)
	if !report.ProcessAlive || !report.ListenerReady {
		t.Fatalf("health = %+v, logs: %v", report, instance.Logs())
	}

	if err := instance.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	if instance.Alive() {
		t.Fatal("real core still alive after Close()")
	}
}

// redactOutput scrubs credential material from captured core output
// before it reaches the test log.
func redactOutput(text string, cfg config.Config) string {
	return core.RedactLogText(text, cfg.SecretFields())
}

// DecodeInboundPort extracts the local inbound port from a generated
// document (both dialects) — used by smoke assertions.
func DecodeInboundPort(t *testing.T, data []byte) int {
	t.Helper()

	var doc struct {
		Inbounds []map[string]any `json:"inbounds"`
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode document: %v", err)
	}

	if len(doc.Inbounds) == 0 {
		t.Fatal("no inbounds")
	}

	if value, ok := doc.Inbounds[0]["port"].(float64); ok {
		return int(value)
	}

	if value, ok := doc.Inbounds[0]["listen_port"].(float64); ok {
		return int(value)
	}

	return 0
}
