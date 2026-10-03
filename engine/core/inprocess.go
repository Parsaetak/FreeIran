package core

import (
	"context"
	"fmt"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// InProcessRuntime is the contract an in-process backend (v0.13.1:
// the first-party FreeIran Engine) fulfills in place of a managed
// child process. It keeps the ONE Instance lifecycle: the instance
// owns the runtime, Close stops it, WaitProcess observes its end.
type InProcessRuntime interface {
	// Stop shuts the runtime down (idempotent; bounded by grace).
	Stop(grace time.Duration)

	// Wait blocks until the runtime stops. A nil return is a clean
	// stop; a non-nil return is a real failure the connection
	// manager must observe — the same semantics a core process
	// exit has. ctx cancellation unblocks the caller only.
	Wait(ctx context.Context) error
}

// nilProcessWaiter adapts the shared readiness supervisor to a launch
// with no process: there is nothing to watch for early exit, so the
// waiter blocks on the launch context exactly like a healthy process
// would.
type nilProcessWaiter struct {
	ctx context.Context
}

// Wait implements processWaiter.
func (n nilProcessWaiter) Wait(ctx context.Context) error {
	<-ctx.Done()

	return ctx.Err()
}

// LaunchInProcess is the shared launch sequence for in-process
// backends (v0.13.1): the runtime's listeners are ALREADY bound (the
// backend binds before calling), the instance wraps the runtime with
// no managed process, and the shared readiness supervisor probes the
// listener through the same awaitListener verdict every backend uses.
//
// The instance differs from a process-backed one in exactly the ways
// the architecture requires: PID is 0 (there is no child), Health
// reports the in-process session's aliveness, and WaitProcess blocks
// until the runtime stops (a clean stop is NOT a crash — the monitor
// contract treats a nil Wait error as "no action").
func LaunchInProcess(
	ctx context.Context,
	backend Core,
	cfg config.Config,
	opts RuntimeOptions,
	doc RuntimeConfig,
	listen string,
	runtime InProcessRuntime,
) (*Instance, error) {
	opts = opts.WithDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindInvalidInput,
			Subsystem, "launch", "invalid configuration")
	}

	if listen == "" {
		return nil, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "launch",
			"in-process backend must bind its listener before launch")
	}

	if runtime == nil {
		return nil, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "launch", "in-process runtime is nil")
	}

	// The log buffer keeps redaction parity with process-backed
	// backends even though the engine writes structured records, not
	// stdout.
	logs := NewLogBuffer(cfg)

	// The session-descriptor artifact plays the same role as a
	// generated core document: the diagnostics-facing record of what
	// this session runs, removed on Close.
	runCfg, err := NewRunConfig(opts.WorkDir, backend.Name())
	if err != nil {
		return nil, err
	}

	if err := runCfg.Write(doc); err != nil {
		_ = runCfg.Cleanup()

		return nil, err
	}

	instance := &Instance{
		core:      backend,
		cfg:       cfg,
		opts:      opts,
		logs:      logs,
		runCfg:    runCfg,
		listen:    listen,
		started:   time.Now().UTC(),
		ready:     make(chan struct{}),
		state:     StateStarting,
		inProcess: runtime,
	}

	logging.LogR(logging.Record{
		Level:     logging.LevelDebug,
		Subsystem: "core",
		Event:     "core_start",
		Lifecycle: true,
		Message:   fmt.Sprintf("core %s starting (in-process)", backend.Name()),
		Core:      backend.Name(),
		Listener:  listen,
		Status:    "starting",
	})

	// THE startup supervision path — the same single observer and
	// the same verdict publication every backend launch uses. The
	// listener is already bound, so the first probe decides.
	go func() {
		outcome, readyErr := awaitListener(ctx, instance.listen, opts.StartupTimeout, nilProcessWaiter{ctx: ctx})

		switch outcome {
		case readinessTimedOut:
			instance.setState(StateTimedOut)

			readyErr = firerrors.New(firerrors.KindDependencyUnavailable,
				Subsystem, "wait_ready",
				"core %s did not become ready within %s (in-process)", instance.CoreName(), opts.StartupTimeout)

		case readinessCancelled:
			readyErr = firerrors.Wrap(context.Cause(ctx), firerrors.KindRetryable,
				Subsystem, "wait_ready", "startup cancelled")
		}

		instance.markReady(readyErr)
	}()

	return instance, nil
}
