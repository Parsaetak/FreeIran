package core

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// fakeInProcess is a minimal InProcessRuntime: a bound listener and a
// controllable stop.
type fakeInProcess struct {
	listener net.Listener
	stopped  chan struct{}
	stopErr  error
}

func newFakeInProcess(t *testing.T) *fakeInProcess {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	return &fakeInProcess{listener: ln, stopped: make(chan struct{})}
}

func (f *fakeInProcess) Stop(time.Duration) {
	select {
	case <-f.stopped:
	default:
		close(f.stopped)
	}
}

func (f *fakeInProcess) Wait(ctx context.Context) error {
	select {
	case <-f.stopped:
		return f.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fakeInProcessBackend adapts the fake runtime to the Core contract.
type fakeInProcessBackend struct {
	runtime *fakeInProcess
}

func (b *fakeInProcessBackend) Name() string { return "fake-inproc" }

func (b *fakeInProcessBackend) Supports(config.Config) bool { return true }

func (b *fakeInProcessBackend) Validate(context.Context, config.Config) error { return nil }

func (b *fakeInProcessBackend) BuildConfig(config.Config, RuntimeOptions) (RuntimeConfig, error) {
	return RuntimeConfig{FileName: "fake.json", Data: []byte("{}"), Format: "fake"}, nil
}

func (b *fakeInProcessBackend) Start(ctx context.Context, cfg config.Config, opts RuntimeOptions) (*Instance, error) {
	return LaunchInProcess(ctx, b, cfg, opts, RuntimeConfig{
		FileName: "fake.json", Data: []byte("{}"), Format: "fake",
	}, b.runtime.listener.Addr().String(), b.runtime)
}

func TestLaunchInProcessLifecycle(t *testing.T) {
	runtime := newFakeInProcess(t)
	backend := &fakeInProcessBackend{runtime: runtime}

	instance, err := backend.Start(context.Background(), config.Config{
		Type: config.TypeSOCKS, Address: "127.0.0.1", Port: 1080,
	}, RuntimeOptions{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// No child process, ever.
	if instance.PID() != 0 {
		t.Fatalf("PID = %d, want 0", instance.PID())
	}

	if instance.Alive() != true {
		t.Fatal("a starting/running in-process instance is alive")
	}

	// Readiness through the shared launch verdict: the listener is
	// already bound, so the first probe decides.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := instance.WaitReady(ctx); err != nil {
		t.Fatalf("wait ready: %v", err)
	}

	// Health: the in-process session is the "process" axis.
	report := instance.Health(ctx)
	if !report.ProcessAlive || !report.ListenerReady {
		t.Fatalf("health = %+v", report)
	}

	// WaitProcess blocks until the runtime stops; a clean stop is nil
	// (NOT a crash).
	waitDone := make(chan error, 1)

	go func() { waitDone <- instance.WaitProcess(context.Background()) }()

	runtime.Stop(0)

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("clean stop WaitProcess = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitProcess did not observe the stop")
	}

	// Close is idempotent and cleans the run-config artifact.
	if err := instance.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := instance.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	if instance.Alive() {
		t.Fatal("closed instance is not alive")
	}
}

func TestWaitProcessReportsRuntimeFailure(t *testing.T) {
	runtime := newFakeInProcess(t)
	runtime.stopErr = errors.New("engine-level failure")

	backend := &fakeInProcessBackend{runtime: runtime}

	instance, err := backend.Start(context.Background(), config.Config{
		Type: config.TypeSOCKS, Address: "127.0.0.1", Port: 1080,
	}, RuntimeOptions{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	defer instance.Close()

	waitDone := make(chan error, 1)

	go func() { waitDone <- instance.WaitProcess(context.Background()) }()

	runtime.Stop(0)

	select {
	case err := <-waitDone:
		if err == nil || err.Error() != "engine-level failure" {
			t.Fatalf("WaitProcess = %v, want the runtime failure (crash semantics)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitProcess did not observe the stop")
	}
}

func TestLaunchInProcessRequiresBoundListener(t *testing.T) {
	backend := &fakeInProcessBackend{runtime: newFakeInProcess(t)}

	_, err := LaunchInProcess(context.Background(), backend, config.Config{
		Type: config.TypeSOCKS, Address: "127.0.0.1", Port: 1080,
	}, RuntimeOptions{}, RuntimeConfig{}, "", backend.runtime)

	if err == nil {
		t.Fatal("LaunchInProcess must refuse an empty listen address")
	}

	_, err = LaunchInProcess(context.Background(), backend, config.Config{
		Type: config.TypeSOCKS, Address: "127.0.0.1", Port: 1080,
	}, RuntimeOptions{}, RuntimeConfig{}, "127.0.0.1:1", nil)

	if err == nil {
		t.Fatal("LaunchInProcess must refuse a nil runtime")
	}
}
