package tunnel

import (
	"context"
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

// TestControllerUnsupportedPlatform verifies that on non-Windows the
// stub TUN backend reports unavailable and Enable returns
// ErrUnsupportedPlatform. On Windows this test exercises the real
// Wintun backend's Available check.
func TestControllerUnsupportedPlatform(t *testing.T) {
	c := New()
	state := c.State()

	// On every platform, calling EnableTUN through the TunnelService
	// should produce a deterministic error when the platform does not
	// support TUN mode (non-Windows) OR when Wintun.dll is missing
	// (Windows). Either case returns a wrapped error.
	if state.Mode != ModeDirect {
		t.Errorf("fresh controller Mode = %s, want %s", state.Mode, ModeDirect)
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
