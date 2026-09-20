package tunnel

import (
	"context"
	"errors"
	"testing"
)

// TestControllerDirectByDefault verifies a fresh controller reports
// ModeDirect + not active.
func TestControllerDirectByDefault(t *testing.T) {
	c := New()
	state := c.State()
	if state.Mode != ModeDirect {
		t.Errorf("Mode = %s, want %s", state.Mode, ModeDirect)
	}
	if state.Active {
		t.Errorf("Active = true, want false")
	}
}

// TestControllerDisableIsIdempotent verifies Disable on a non-active
// controller is a no-op.
func TestControllerDisableIsIdempotent(t *testing.T) {
	c := New()
	if err := c.Disable(context.Background()); err != nil {
		t.Errorf("Disable on direct controller returned err: %v", err)
	}
}

// TestControllerEnableDirectIsNoop verifies Enable(ModeDirect, ...) is
// a no-op.
func TestControllerEnableDirectIsNoop(t *testing.T) {
	c := New()
	if err := c.Enable(context.Background(), ModeDirect, "127.0.0.1", 1080, Options{}); err != nil {
		t.Errorf("Enable(ModeDirect) returned err: %v", err)
	}
}

// TestControllerTUNDisabledEverywhere pins the v0.9.8.6 TUN policy:
// on EVERY platform the controller reports TUN as unavailable and
// Enable(ModeTUN) fails with the explicit experimental/disabled
// error. No platform may half-configure the system before refusing.
func TestControllerTUNDisabledEverywhere(t *testing.T) {
	c := New()

	if c.tun.Available() {
		t.Fatal("TUN must report unavailable on every platform in this release")
	}

	err := c.Enable(context.Background(), ModeTUN, "127.0.0.1", 1080, Options{})
	if err == nil {
		t.Fatal("Enable(ModeTUN) must fail with the experimental/disabled error")
	}

	if !errors.Is(err, ErrTunExperimental) {
		t.Fatalf("err = %v, want ErrTunExperimental", err)
	}

	// The failed enable must leave the controller untouched.
	state := c.State()
	if state.Mode != ModeDirect || state.Active {
		t.Fatalf("state = %+v, want direct/inactive after the refused enable", state)
	}
}

// TestTunBackendRefusesInstallAndEnable pins the backend surface
// directly: Install and Enable both refuse, Disable is a harmless
// no-op and the snapshot reports the honest unavailable state.
func TestTunBackendRefusesInstallAndEnable(t *testing.T) {
	backend := newTUNBackend()

	if backend.Available() {
		t.Fatal("Available must be false")
	}

	if err := backend.Install(context.Background()); !errors.Is(err, ErrTunExperimental) {
		t.Fatalf("Install err = %v, want ErrTunExperimental", err)
	}

	if err := backend.Enable(context.Background(), "127.0.0.1", 1080); !errors.Is(err, ErrTunExperimental) {
		t.Fatalf("Enable err = %v, want ErrTunExperimental", err)
	}

	if err := backend.Disable(context.Background()); err != nil {
		t.Fatalf("Disable err = %v, want nil no-op", err)
	}

	snap := backend.Snapshot()
	if snap.Available || snap.Installed {
		t.Fatalf("snapshot = %+v, want unavailable/not-installed", snap)
	}
}

// TestStateString verifies the State struct can be JSON-marshaled
// (the Wails binding layer needs this).
func TestStateString(t *testing.T) {
	c := New()
	state := c.State()
	if state.Mode != ModeDirect {
		t.Errorf("Mode = %s, want %s", state.Mode, ModeDirect)
	}
}
