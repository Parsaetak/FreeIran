package v2ray_test

import (
	"os"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
)

// TestV2RaySmokeRealBinary validates the V2Ray adapter against a
// real V2Fly v2ray-core binary. Skipped unless
// FREEIRAN_TEST_V2RAY_BIN points at the executable — CI installs the
// pinned release (v5.53.0, SHA-256 verified) and runs this test; no
// external server is contacted (the smoke test exercises config
// acceptance, startup and listener readiness only).
func TestV2RaySmokeRealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode skips real-binary smoke tests")
	}

	contract.RunSmoke(t, contract.SmokeOptions{
		BinaryPath: os.Getenv("FREEIRAN_TEST_V2RAY_BIN"),
		Backend:    newBackend(),
		Configs:    smokeConfigs(),
		ValidateArgs: func(file string) []string {
			return []string{"test", "-c", file}
		},
		Timeout: 20 * time.Second,
	})
}

// newBackend isolates the smoke fixture from other files.
func newBackend() core.Core {
	return v2ray.New()
}

// smokeConfigs covers every protocol family the V2Ray adapter
// supports, with the transports verified against v5.53.0.
func smokeConfigs() []config.Config {
	return []config.Config{
		vlessTLSConfig(),
		vlessWSConfig(),
		vmessGRPCConfig(),
		trojanConfig(),
		shadowsocksConfig(),
		socksConfig(),
		httpConfig(),
	}
}
