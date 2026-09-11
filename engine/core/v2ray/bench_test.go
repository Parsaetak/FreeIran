package v2ray_test

import (
	"context"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
)

// BenchmarkBuildConfig measures V2Ray runtime-document generation
// (the V4 dialect shared with Xray).
func BenchmarkBuildConfig(b *testing.B) {
	backend := v2ray.New()

	opts := core.RuntimeOptions{
		LocalPort:       45090,
		DisableGenCache: true,
	}

	cases := []config.Config{
		{Type: config.TypeVLESS, Address: "bench.example.org", Port: 443,
			UUID: "11111111-1111-1111-1111-111111111111", Network: "tcp", Security: "tls"},
		{Type: config.TypeVLESS, Address: "bench.example.org", Port: 443,
			UUID: "11111111-1111-1111-1111-111111111111", Network: "ws", Security: "tls",
			Path: "/tunnel", Host: "bench.example.org"},
		{Type: config.TypeVMess, Address: "bench.example.org", Port: 443,
			UUID: "22222222-2222-2222-2222-222222222222", Network: "grpc", Security: "tls", Service: "svc"},
		{Type: config.TypeTrojan, Address: "bench.example.org", Port: 443,
			Password: "bench-password", Network: "tcp", Security: "tls"},
	}

	for _, cfg := range cases {
		b.Run(string(cfg.Type)+"_"+cfg.Network, func(b *testing.B) {
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				if _, err := backend.BuildConfig(cfg, opts); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBuildConfigCached measures generation with the runtime
// generation cache warm (the reconnect path).
func BenchmarkBuildConfigCached(b *testing.B) {
	backend := v2ray.New()

	opts := core.RuntimeOptions{LocalPort: 45090}

	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "bench.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "tcp",
		Security: "tls",
	}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if _, err := backend.BuildConfig(cfg, opts); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkValidate measures capability validation.
func BenchmarkValidate(b *testing.B) {
	backend := v2ray.New()

	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "bench.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "ws",
		Security: "tls",
	}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := backend.Validate(context.Background(), cfg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSupports measures the hot capability check used by
// selection and the details view.
func BenchmarkSupports(b *testing.B) {
	backend := v2ray.New()

	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "bench.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "ws",
		Security: "tls",
	}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if !backend.Supports(cfg) {
			b.Fatal("expected support")
		}
	}
}

// BenchmarkFingerprint measures the normalized-model fingerprint
// (dedup identity path).
func BenchmarkFingerprint(b *testing.B) {
	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "bench.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "ws",
		Security: "tls",
		Path:     "/tunnel",
		Host:     "bench.example.org",
	}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = cfg.Fingerprint()
	}
}

// BenchmarkNormalize measures normalization (pipeline hot path).
func BenchmarkNormalize(b *testing.B) {
	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "Bench.Example.ORG ",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  " WS ",
		Security: " TLS ",
	}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cfg.Normalize()
	}
}

// BenchmarkDisplayURL measures redacted display rendering used by
// every UI surface and diagnostic.
func BenchmarkDisplayURL(b *testing.B) {
	cfg := config.Config{
		Type:     config.TypeVLESS,
		Address:  "bench.example.org",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "ws",
		Security: "tls",
	}

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = cfg.DisplayURL()
	}
}
