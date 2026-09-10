package tester

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func TestTCPProbeReachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}

	defer listener.Close()

	go func() {
		conn, _ := listener.Accept()

		if conn != nil {
			_ = conn.Close()
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port

	probe := NewTCPProbe()

	cfg := config.Config{
		Type:    config.TypeVLESS,
		Address: "127.0.0.1",
		Port:    port,
	}

	result, err := probe.Test(context.Background(), cfg)
	if err != nil {
		t.Fatalf("test: %v", err)
	}

	if !result.Working {
		t.Fatalf("endpoint should be reachable: %s", result.LastError)
	}

	if result.Latency <= 0 {
		t.Fatal("latency should be measured")
	}
}

func TestTCPProbeUnreachable(t *testing.T) {
	probe := NewTCPProbe()

	cfg := config.Config{
		Type:    config.TypeTrojan,
		Address: "127.0.0.1",
		Port:    1,
	}

	result, err := probe.Test(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unreachable must be a Result, not an error: %v", err)
	}

	if result.Working {
		t.Fatal("port 1 must not be reachable")
	}

	if result.LastError == "" {
		t.Fatal("unreachable result should carry a reason")
	}
}

func TestTCPProbeSupportsEverything(t *testing.T) {
	probe := NewTCPProbe()

	for _, protocol := range []config.Type{
		config.TypeVLESS, config.TypeVMess, config.TypeTrojan,
		config.TypeShadowsocks, config.TypeHysteria, config.TypeHysteria2,
		config.TypeTUIC, config.TypeWireGuard, config.TypeSOCKS,
		config.TypeHTTP,
	} {
		if !probe.Supports(protocol) {
			t.Fatalf("TCP probe must support %s", protocol)
		}
	}
}

func TestTCPProbeRespectsTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}

	defer listener.Close()

	// Listener accepts but never responds; a TCP connect DOES
	// complete for backlog'd listeners, so use an unroutable address
	// with a short timeout instead.
	probe := TCPProbe{Timeout: 200 * time.Millisecond}

	cfg := config.Config{
		Type:    config.TypeVLESS,
		Address: "10.255.255.1", // unroutable in test environments
		Port:    65534,
	}

	started := time.Now()

	_, _ = probe.Test(context.Background(), cfg)

	elapsed := time.Since(started)

	if elapsed > 3*time.Second {
		t.Fatalf("probe took %v; timeout not respected", elapsed)
	}
}
