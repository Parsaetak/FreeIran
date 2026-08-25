package core

import (
	"context"
	"errors"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

type fakeInstance struct {
	endpoint string
	closed   bool
}

func (i *fakeInstance) Endpoint() string {
	return i.endpoint
}

func (i *fakeInstance) Close() error {
	i.closed = true
	return nil
}

type fakeCore struct {
	protocol config.Type
	instance Instance
	err      error
}

func (c *fakeCore) Type() config.Type {
	return c.protocol
}

func (c *fakeCore) Start(
	_ context.Context,
	_ config.Config,
) (Instance, error) {
	return c.instance, c.err
}

func validConfig() config.Config {
	return config.Config{
		Type:    config.TypeVLESS,
		Address: "example.com",
		Port:    443,
		UUID:    "11111111-1111-1111-1111-111111111111",
	}
}

func TestNewRegistry(t *testing.T) {
	registry := NewRegistry()

	if registry == nil {
		t.Fatal("expected registry")
	}

	if registry.cores == nil {
		t.Fatal("expected initialized core map")
	}

	if len(registry.cores) != 0 {
		t.Fatalf(
			"expected empty registry, got %d cores",
			len(registry.cores),
		)
	}
}

func TestRegisterAndGet(t *testing.T) {
	registry := NewRegistry()

	core := &fakeCore{
		protocol: config.TypeVLESS,
	}

	if err := registry.Register(core); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	got, ok := registry.Get(config.TypeVLESS)

	if !ok {
		t.Fatal("expected registered core")
	}

	if got != core {
		t.Fatal("returned core differs from registered core")
	}
}

func TestRegisterReplacesExistingCore(t *testing.T) {
	registry := NewRegistry()

	first := &fakeCore{
		protocol: config.TypeVLESS,
	}

	second := &fakeCore{
		protocol: config.TypeVLESS,
	}

	if err := registry.Register(first); err != nil {
		t.Fatalf("first Register() failed: %v", err)
	}

	if err := registry.Register(second); err != nil {
		t.Fatalf("second Register() failed: %v", err)
	}

	got, ok := registry.Get(config.TypeVLESS)

	if !ok {
		t.Fatal("expected registered core")
	}

	if got != second {
		t.Fatal("expected second core to replace first")
	}
}

func TestRegisterRejectsNilCore(t *testing.T) {
	registry := NewRegistry()

	if err := registry.Register(nil); err == nil {
		t.Fatal("expected nil core to be rejected")
	}
}

func TestRegisterRejectsUnknownProtocol(t *testing.T) {
	registry := NewRegistry()

	core := &fakeCore{
		protocol: config.TypeUnknown,
	}

	if err := registry.Register(core); err == nil {
		t.Fatal("expected unknown protocol to be rejected")
	}
}

func TestSupports(t *testing.T) {
	registry := NewRegistry()

	if registry.Supports(config.TypeVLESS) {
		t.Fatal("unexpected VLESS support")
	}

	if err := registry.Register(&fakeCore{
		protocol: config.TypeVLESS,
	}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	if !registry.Supports(config.TypeVLESS) {
		t.Fatal("expected VLESS support")
	}

	if registry.Supports(config.TypeVMess) {
		t.Fatal("unexpected VMess support")
	}
}

func TestStart(t *testing.T) {
	instance := &fakeInstance{
		endpoint: "127.0.0.1:1080",
	}

	registry := NewRegistry()

	if err := registry.Register(&fakeCore{
		protocol: config.TypeVLESS,
		instance: instance,
	}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	got, err := registry.Start(
		context.Background(),
		validConfig(),
	)

	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if got != instance {
		t.Fatal("unexpected instance returned")
	}

	if got.Endpoint() != "127.0.0.1:1080" {
		t.Fatalf(
			"unexpected endpoint: %s",
			got.Endpoint(),
		)
	}
}

func TestStartRejectsInvalidConfiguration(t *testing.T) {
	registry := NewRegistry()

	if err := registry.Register(&fakeCore{
		protocol: config.TypeVLESS,
	}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	cfg := validConfig()
	cfg.UUID = ""

	if _, err := registry.Start(
		context.Background(),
		cfg,
	); err == nil {
		t.Fatal("expected invalid configuration to be rejected")
	}
}

func TestStartRejectsUnsupportedProtocol(t *testing.T) {
	registry := NewRegistry()

	cfg := validConfig()
	cfg.Type = config.TypeVMess

	if _, err := registry.Start(
		context.Background(),
		cfg,
	); err == nil {
		t.Fatal("expected unsupported protocol to be rejected")
	}
}

func TestStartPropagatesCoreError(t *testing.T) {
	expected := errors.New("core startup failed")

	registry := NewRegistry()

	if err := registry.Register(&fakeCore{
		protocol: config.TypeVLESS,
		err:      expected,
	}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	_, err := registry.Start(
		context.Background(),
		validConfig(),
	)

	if err == nil {
		t.Fatal("expected startup error")
	}

	if !errors.Is(err, expected) {
		t.Fatalf(
			"expected wrapped startup error, got %v",
			err,
		)
	}
}

func TestStartRejectsNilInstance(t *testing.T) {
	registry := NewRegistry()

	if err := registry.Register(&fakeCore{
		protocol: config.TypeVLESS,
	}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	_, err := registry.Start(
		context.Background(),
		validConfig(),
	)

	if err == nil {
		t.Fatal("expected nil instance error")
	}
}

func TestInstanceClose(t *testing.T) {
	instance := &fakeInstance{
		endpoint: "127.0.0.1:1080",
	}

	if err := instance.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	if !instance.closed {
		t.Fatal("expected instance to be closed")
	}
}
