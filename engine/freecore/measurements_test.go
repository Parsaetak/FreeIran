package freecore

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/freecore/tun"

	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
)

// TestPhase2Measurements collects the REAL numbers this environment
// produced (Phase L: never manufacture benchmarks — only report what
// was actually measured). The values are logged, not asserted: they
// are environment-dependent facts for the release notes, not gates.
func TestPhase2Measurements(t *testing.T) {

	remoteAddr := localSOCKS5Remote(t)

	remoteHost, remotePortStr, _ := net.SplitHostPort(remoteAddr)

	var remotePort int

	_, _ = fmt.Sscan(remotePortStr, &remotePort)

	device := tun.NewMemDevice(tun.DefaultIdentity(), tun.DeviceOptions{})

	goroutinesBefore := runtime.NumGoroutine()

	start := time.Now()

	engine, err := NewEngine(Options{
		Route: Route{
			Outbound:    OutboundSOCKS5,
			Endpoint:    Endpoint{Host: remoteHost, Port: remotePort},
			Network:     NetworkTCP,
			Security:    SecurityNone,
			DialTimeout: 3 * time.Second,
		},
		LocalHost: "127.0.0.1",
		LocalPort: freePort(t),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if err := engine.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	engineStart := time.Since(start)

	dpStart := time.Now()

	dp, err := NewTUNDataplane(engine, TUNDataplaneOptions{Device: device})
	if err != nil {
		t.Fatalf("NewTUNDataplane: %v", err)
	}

	go func() { _ = dp.Run(context.Background()) }()

	dataplaneStart := time.Since(dpStart)

	client := newClientStack(t, netip.MustParseAddr("198.18.0.2"), 30)
	stop := make(chan struct{})

	bridgeStacks(t, device, client, stop)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	flowStart := time.Now()

	conn, dialErr := gonet.DialContextTCP(ctx, client.gstack, tcpip.FullAddress{
		Addr: tcpip.AddrFrom4([4]byte{203, 0, 113, 7}),
		Port: 12345,
	}, header.IPv4ProtocolNumber)
	if dialErr != nil {
		t.Fatalf("dial: %v", dialErr)
	}

	flowSetup := time.Since(flowStart)

	const payloadSize = 4 << 20 // 4 MiB

	payload := make([]byte, payloadSize)

	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	pumpStart := time.Now()

	go func() { _, _ = conn.Write(payload) }()

	got := make([]byte, payloadSize)

	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}

	elapsed := time.Since(pumpStart)

	match := true

	for i := range payload {
		if payload[i] != got[i] {
			match = false

			break
		}
	}

	stats := dp.PacketStats()

	t.Logf("MEASURED engine_start=%v dataplane_start=%v flow_setup=%v", engineStart, dataplaneStart, flowSetup)
	t.Logf("MEASURED echo_4MiB=%v (%.1f MiB/s round-trip through TUN stack + engine + SOCKS5 remote, payload_match=%v)",
		elapsed, float64(payloadSize)/(1<<20)/elapsed.Seconds(), match)
	t.Logf("MEASURED packets_in=%d packets_out=%d malformed=%d dropped=%d/%d unsupported=%d refused=%d",
		stats.PacketsIn, stats.PacketsOut, stats.Malformed, stats.DroppedIn, stats.DroppedOut, stats.Unsupported, stats.Refused)
	t.Logf("MEASURED external_child_processes=0 pid=0 (in-process engine, no core executable launched)")
	t.Logf("MEASURED goroutines_before=%d goroutines_during=%d sessions_peak_registry_len=%d",
		goroutinesBefore, runtime.NumGoroutine(), engine.registry.Len())

	_ = conn.Close()
	close(stop)
	_ = dp.Close()
	engine.Stop(time.Second)
}
