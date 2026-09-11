// Package contract provides the shared test suite every protocol
// core backend must satisfy. Running the same suite against the
// Xray, V2Ray and sing-box adapters keeps the three implementations
// from diverging: the same lifecycle, the same failure semantics,
// the same determinism guarantees.
//
// Process-level tests execute the fake core helper
// (engine/core/testdata/fakecore) as the binary stand-in, so the
// suite exercises real spawn/readiness/stop/cleanup behaviour
// without requiring a protocol core to be installed.
package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/system"
)

// Case is one capability expectation for a backend.
type Case struct {
	Name     string
	Config   config.Config
	Supports bool
	Valid    bool
}

// Suite bundles everything a backend must pass.
type Suite struct {
	Backend core.Core

	// Supported cases must pass Supports AND Validate AND
	// BuildConfig.
	Supported []Case

	// Unsupported cases must fail Supports (and Validate).
	Unsupported []Case

	// FakeBinary is the process stand-in for lifecycle tests
	// (built through BuildFakeCore).
	FakeBinary string
}

// Run executes the full contract suite.
func Run(t *testing.T, suite Suite) {
	t.Helper()

	backend := suite.Backend

	if backend == nil {
		t.Fatal("contract: backend is nil")
	}

	if len(suite.Supported) == 0 {
		t.Fatal("contract: no supported cases supplied")
	}

	t.Run("identity", func(t *testing.T) {
		if backend.Name() == "" {
			t.Fatal("Name() is empty")
		}
	})

	t.Run("supports", func(t *testing.T) {
		for _, testCase := range suite.Supported {
			if !backend.Supports(testCase.Config) {
				t.Errorf("%s: Supports(%s) = false, want true",
					testCase.Name, describe(testCase.Config))
			}
		}

		for _, testCase := range suite.Unsupported {
			if backend.Supports(testCase.Config) {
				t.Errorf("%s: Supports(%s) = true, want false",
					testCase.Name, describe(testCase.Config))
			}
		}
	})

	t.Run("validate", func(t *testing.T) {
		ctx := context.Background()

		for _, testCase := range suite.Supported {
			if err := backend.Validate(ctx, testCase.Config); err != nil {
				t.Errorf("%s: Validate() = %v, want nil", testCase.Name, err)
			}
		}

		for _, testCase := range suite.Unsupported {
			if err := backend.Validate(ctx, testCase.Config); err == nil {
				t.Errorf("%s: Validate() = nil, want error", testCase.Name)
			}
		}

		// Hostile inputs must never panic.
		hostile := []config.Config{
			{Type: config.TypeVLESS, Address: strings.Repeat("a", 4096), Port: 99999},
			{Type: config.Type(""), Address: "", Port: -1},
			{Type: config.TypeVMess, Address: "\x00\x01\xff", Port: 0},
			{Type: config.TypeTrojan, Address: "ok.example", Port: 443, Password: strings.Repeat("p", 1<<16)},
		}

		for i, cfg := range hostile {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("hostile config %d panicked: %v", i, r)
					}
				}()

				_ = backend.Validate(ctx, cfg) //nolint:errcheck // panic test
				_ = backend.Supports(cfg)      //nolint:errcheck // panic test
			}()
		}
	})

	t.Run("build_deterministic", func(t *testing.T) {
		for _, testCase := range suite.Supported {
			opts := core.RuntimeOptions{
				LocalPort:       45080,
				DisableGenCache: true,
			}

			first, err := backend.BuildConfig(testCase.Config, opts)
			if err != nil {
				t.Errorf("%s: BuildConfig() = %v", testCase.Name, err)

				continue
			}

			second, err := backend.BuildConfig(testCase.Config, opts)
			if err != nil {
				t.Errorf("%s: BuildConfig() second = %v", testCase.Name, err)

				continue
			}

			if string(first.Data) != string(second.Data) {
				t.Errorf("%s: BuildConfig is not deterministic", testCase.Name)
			}
		}
	})

	t.Run("build_structural", func(t *testing.T) {
		for _, testCase := range suite.Supported {
			opts := core.RuntimeOptions{LocalPort: 45081}

			doc, err := backend.BuildConfig(testCase.Config, opts)
			if err != nil {
				t.Errorf("%s: BuildConfig() = %v", testCase.Name, err)

				continue
			}

			var generic map[string]any

			if err := json.Unmarshal(doc.Data, &generic); err != nil {
				t.Errorf("%s: generated document is not valid JSON: %v",
					testCase.Name, err)

				continue
			}

			if len(generic) == 0 {
				t.Errorf("%s: generated document is empty", testCase.Name)
			}

			if doc.FileName == "" {
				t.Errorf("%s: document has no file name", testCase.Name)
			}

			// The redacted summary must never contain credentials.
			assertNoSecrets(t, testCase.Name, doc.RedactedSummary, testCase.Config)

			// The document must contain the local inbound port.
			assertInboundPort(t, testCase.Name, doc.Data, 45081)
		}
	})

	t.Run("build_requires_port", func(t *testing.T) {
		opts := core.RuntimeOptions{DisableGenCache: true}

		_, err := backend.BuildConfig(suite.Supported[0].Config, opts)
		if err == nil {
			t.Fatal("BuildConfig without a local port should fail")
		}
	})

	if suite.FakeBinary == "" {
		t.Log("no fake binary supplied; lifecycle tests skipped")

		return
	}

	runLifecycleSuite(t, suite)
}

// runLifecycleSuite exercises the process lifecycle against the fake
// core binary: start, readiness, health, stop, cleanup — and the
// failure paths.
func runLifecycleSuite(t *testing.T, suite Suite) {
	backend := suite.Backend

	cfg := suite.Supported[0].Config

	t.Run("start_ready_stop", func(t *testing.T) {
		port := reservePort(t)

		opts := core.RuntimeOptions{
			LocalPort:       port,
			BinaryPath:      suite.FakeBinary,
			StartupTimeout:  10 * time.Second,
			DisableGenCache: true,
			WorkDir:         t.TempDir(),
		}

		instance, err := backend.Start(context.Background(), cfg, opts)
		if err != nil {
			t.Fatalf("Start() = %v", err)
		}

		if err := instance.WaitReady(context.Background()); err != nil {
			t.Fatalf("WaitReady() = %v", err)
		}

		if state := instance.State(); state != core.StateRunning {
			t.Fatalf("state = %s, want running", state)
		}

		report := instance.Health(context.Background())
		if !report.ProcessAlive || !report.ListenerReady {
			t.Fatalf("health = %+v, want alive+ready", report)
		}

		if instance.PID() <= 0 {
			t.Fatalf("pid = %d, want > 0", instance.PID())
		}

		if instance.Endpoint() == "" {
			t.Fatal("Endpoint() is empty after readiness")
		}

		if err := instance.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}

		if state := instance.State(); state != core.StateStopped {
			t.Fatalf("state after close = %s, want stopped", state)
		}

		if instance.Alive() {
			t.Fatal("process still alive after Close()")
		}

		// Idempotent close.
		if err := instance.Close(); err != nil {
			t.Fatalf("second Close() = %v", err)
		}
	})

	t.Run("temp_config_cleaned", func(t *testing.T) {
		workDir := t.TempDir()

		port := reservePort(t)

		opts := core.RuntimeOptions{
			LocalPort:       port,
			BinaryPath:      suite.FakeBinary,
			DisableGenCache: true,
			WorkDir:         workDir,
		}

		instance, err := backend.Start(context.Background(), cfg, opts)
		if err != nil {
			t.Fatalf("Start() = %v", err)
		}

		if err := instance.WaitReady(context.Background()); err != nil {
			t.Fatalf("WaitReady() = %v", err)
		}

		if err := instance.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}

		// Caller-owned workDir must contain no leftover config files.
		assertNoLeftoverConfigs(t, workDir)
	})

	t.Run("startup_failure_cleans_temp", func(t *testing.T) {
		workDir := t.TempDir()

		port := reservePort(t)

		opts := core.RuntimeOptions{
			LocalPort:       port,
			BinaryPath:      suite.FakeBinary,
			DisableGenCache: true,
			WorkDir:         workDir,
			Env:             []string{"FAKECORE_FAIL_FAST=1"},
		}

		instance, err := backend.Start(context.Background(), cfg, opts)
		if err != nil {
			t.Fatalf("Start() = %v", err)
		}

		if err := instance.WaitReady(context.Background()); err == nil {
			t.Fatal("WaitReady() should fail when the core exits at startup")
		}

		if err := instance.Close(); err != nil {
			t.Fatalf("Close() after failed start = %v", err)
		}

		assertNoLeftoverConfigs(t, workDir)
	})

	t.Run("missing_binary", func(t *testing.T) {
		opts := core.RuntimeOptions{
			LocalPort:       reservePort(t),
			BinaryPath:      filepath.Join(t.TempDir(), "does-not-exist"),
			DisableGenCache: true,
		}

		_, err := backend.Start(context.Background(), cfg, opts)
		if err == nil {
			t.Fatal("Start() with a missing binary should fail")
		}
	})

	t.Run("no_binary_path", func(t *testing.T) {
		opts := core.RuntimeOptions{LocalPort: reservePort(t)}

		_, err := backend.Start(context.Background(), cfg, opts)
		if err == nil {
			t.Fatal("Start() without a binary path should fail")
		}
	})

	t.Run("rapid_start_stop", func(t *testing.T) {
		for cycle := 0; cycle < 3; cycle++ {
			port := reservePort(t)

			opts := core.RuntimeOptions{
				LocalPort:       port,
				BinaryPath:      suite.FakeBinary,
				StartupTimeout:  10 * time.Second,
				DisableGenCache: true,
				WorkDir:         t.TempDir(),
			}

			instance, err := backend.Start(context.Background(), cfg, opts)
			if err != nil {
				t.Fatalf("cycle %d: Start() = %v", cycle, err)
			}

			if err := instance.WaitReady(context.Background()); err != nil {
				t.Fatalf("cycle %d: WaitReady() = %v", cycle, err)
			}

			if err := instance.Close(); err != nil {
				t.Fatalf("cycle %d: Close() = %v", cycle, err)
			}
		}
	})

	t.Run("concurrent_close", func(t *testing.T) {
		port := reservePort(t)

		opts := core.RuntimeOptions{
			LocalPort:       port,
			BinaryPath:      suite.FakeBinary,
			StartupTimeout:  10 * time.Second,
			DisableGenCache: true,
			WorkDir:         t.TempDir(),
		}

		instance, err := backend.Start(context.Background(), cfg, opts)
		if err != nil {
			t.Fatalf("Start() = %v", err)
		}

		if err := instance.WaitReady(context.Background()); err != nil {
			t.Fatalf("WaitReady() = %v", err)
		}

		done := make(chan error, 4)

		for i := 0; i < 4; i++ {
			go func() { done <- instance.Close() }()
		}

		for i := 0; i < 4; i++ {
			if err := <-done; err != nil {
				t.Fatalf("concurrent Close() = %v", err)
			}
		}

		if instance.Alive() {
			t.Fatal("process alive after concurrent closes")
		}
	})
}

// BuildFakeCore provides the compiled fake protocol-core binary.
//
// Resolution order (deterministic, CI-friendly):
//
//  1. FREEIRAN_TEST_CORES environment variable: when set, it names a
//     fixture directory containing PRE-BUILT fake cores (the CI jobs
//     build them there: fakecore[.exe], fake-xray[.exe],
//     fake-v2ray[.exe], fake-sing-box[.exe]). A missing fixture is a
//     hard test failure — CI promised the fixture, so silently
//     skipping would hide a broken harness. Tests never depend on
//     whatever happens to be in PATH and never install real cores.
//  2. Otherwise the fake core is compiled from
//     engine/core/testdata/fakecore with the local Go toolchain
//     (developer machines). Without a toolchain the calling test is
//     skipped.
//
// The source path is resolved from THIS file's location through
// runtime.Caller — never from the test's working directory — so the
// helper is buildable from every consuming package (engine/core/*,
// engine/connection, engine/app) regardless of cwd. The compiled
// binary is built once per test process and copied for every caller,
// so suites that spawn the fake core repeatedly stay fast.
func BuildFakeCore(tb testing.TB) string {
	tb.Helper()

	if dir := os.Getenv("FREEIRAN_TEST_CORES"); dir != "" {
		for _, name := range []string{"fakecore", "fake-xray", "fake-v2ray", "fake-sing-box"} {
			candidate := filepath.Join(dir, system.ExecutableName(name))
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return copyFakeCore(tb, candidate)
			}
		}

		tb.Fatalf("FREEIRAN_TEST_CORES=%s contains no fake core fixture "+
			"(expected %s); the CI harness must build it before running tests",
			dir, system.ExecutableName("fakecore"))
	}

	return copyFakeCore(tb, buildFakeCoreOnce(tb))
}

// StageFakeCore copies the fake core into dir under the
// platform-correct executable name for the given core ("v2ray" →
// "v2ray.exe" on Windows) and returns the destination path. All test
// staging MUST go through this helper: staging an extensionless
// binary on Windows makes the registry discovery miss it (the exact
// v0.4.x Windows CI failure).
func StageFakeCore(tb testing.TB, dir, coreName string) string {
	tb.Helper()

	source := BuildFakeCore(tb)

	dest := filepath.Join(dir, system.ExecutableName(coreName))

	copyBinary(tb, source, dest)

	return dest
}

// buildCache serializes the one-per-process compilation.
var buildCache = struct {
	sync.Once
	path string
	err  error
}{}

// buildFakeCoreOnce compiles the fake core helper once per process.
func buildFakeCoreOnce(tb testing.TB) string {
	tb.Helper()

	buildCache.Do(func() {
		goTool, err := exec.LookPath("go")
		if err != nil {
			buildCache.err = err

			return
		}

		shared, err := os.MkdirTemp("", "freeiran-fakecore-")
		if err != nil {
			buildCache.err = err

			return
		}

		binary := filepath.Join(shared, "fakecore")

		if runtime.GOOS == "windows" {
			binary += ".exe"
		}

		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			buildCache.err = errors.New("cannot locate the contract package source")

			return
		}

		source, err := filepath.Abs(filepath.Join(
			filepath.Dir(thisFile), "..", "testdata", "fakecore", "main.go"))
		if err != nil {
			buildCache.err = err

			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		build := exec.CommandContext(ctx, goTool, "build", "-o", binary, source)

		if out, err := build.CombinedOutput(); err != nil {
			buildCache.err = fmt.Errorf("fake core build failed: %v: %s", err, out)

			return
		}

		buildCache.path = binary
	})

	if buildCache.err != nil {
		tb.Skipf("fake core unavailable: %v", buildCache.err)
	}

	return buildCache.path
}

// copyFakeCore copies the shared fake core binary into the caller's
// private temp directory (cleanup is automatic through testing).
func copyFakeCore(tb testing.TB, source string) string {
	tb.Helper()

	dest := filepath.Join(tb.TempDir(), "fakecore")

	if runtime.GOOS == "windows" {
		dest += ".exe"
	}

	copyBinary(tb, source, dest)

	return dest
}

// copyBinary copies an executable and restores its permission bits.
// Re-copying a binary that a previous test already executed is safe:
// the destination is a fresh path per test (on Windows, running a
// file locks only that file's own path, and every test receives its
// own copy).
func copyBinary(tb testing.TB, source, dest string) {
	tb.Helper()

	data, err := os.ReadFile(source)
	if err != nil {
		tb.Fatalf("read fake core %s: %v", source, err)
	}

	if err := os.WriteFile(dest, data, 0o755); err != nil {
		tb.Fatalf("stage fake core %s: %v", dest, err)
	}
}

// assertNoSecrets fails when credential material appears in text.
func assertNoSecrets(t *testing.T, name, text string, cfg config.Config) {
	t.Helper()

	for _, secret := range cfg.SecretFields() {
		if secret != "" && strings.Contains(text, secret) {
			t.Errorf("%s: redacted summary leaks credential material", name)
		}
	}
}

// assertInboundPort verifies the generated document carries the
// local inbound port (V4 "port" or sing-box "listen_port").
func assertInboundPort(t *testing.T, name string, data []byte, want int) {
	t.Helper()

	var doc struct {
		Inbounds []map[string]any `json:"inbounds"`
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		t.Errorf("%s: cannot re-parse document: %v", name, err)

		return
	}

	if len(doc.Inbounds) == 0 {
		t.Errorf("%s: document has no inbounds", name)

		return
	}

	inbound := doc.Inbounds[0]

	port := 0

	if value, ok := inbound["port"].(float64); ok {
		port = int(value)
	}

	if value, ok := inbound["listen_port"].(float64); ok {
		port = int(value)
	}

	if port != want {
		t.Errorf("%s: inbound port = %d, want %d", name, port, want)
	}
}

// assertNoLeftoverConfigs fails when .json files remain in a
// runtime directory after cleanup.
func assertNoLeftoverConfigs(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workdir: %v", err)
	}

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("temporary config %s was not cleaned up", entry.Name())
		}
	}
}

// describe renders a case description for failure messages.
func describe(cfg config.Config) string {
	return string(cfg.Type) + "/" + cfg.Network + "/" + cfg.Security
}

// reservePort allocates a distinct local port for one lifecycle test.
func reservePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port

	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}

	return port
}
