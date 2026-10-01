package v2ray_test

import (
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
)

// TestV2RayChainExplicitlyUnsupported pins the honest v0.12.2
// contract: the V2Fly adapter refuses proxy chains with an explicit
// unsupported error — never an emulated multi-process chain, never a
// silent miscompile.
func TestV2RayChainExplicitlyUnsupported(t *testing.T) {
	hop := config.Config{
		Type:    config.TypeVLESS,
		Name:    "hop-a",
		Address: "a.example.org",
		Port:    443,
		UUID:    "aaaaaaaa-1111-1111-1111-111111111111",
		Network: "tcp",
	}

	exit := config.Config{
		Type:     config.TypeVLESS,
		Name:     "hop-b",
		Address:  "b.example.org",
		Port:     443,
		UUID:     "bbbbbbbb-2222-2222-2222-222222222222",
		Network:  "tcp",
		Security: "tls",
		Chain:    []*config.Config{&hop},
	}

	backend := v2ray.New()

	if backend.Supports(exit) {
		t.Fatal("v2ray adapter reports chain support — must be explicitly unsupported")
	}

	_, err := backend.BuildConfig(exit, core.RuntimeOptions{LocalPort: 45104})
	if err == nil {
		t.Fatal("v2ray chain build succeeded — must be refused")
	}

	if !strings.Contains(err.Error(), "does not support proxy chains") {
		t.Fatalf("unsupported error not actionable: %v", err)
	}
}
