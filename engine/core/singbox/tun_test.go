package singbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
)

// tunDocForTest renders a TUN document for the standard shadowsocks
// fixture (parsed into the loose test map shape).
func tunDocForTest(t *testing.T, cfg config.Config, tun TUNSettings) map[string]any {
	t.Helper()

	opts := core.RuntimeOptions{LocalHost: "127.0.0.1", LocalPort: 18931}

	data, summary, err := BuildTUNDocument(cfg, opts, tun)
	if err != nil {
		t.Fatalf("BuildTUNDocument: %v", err)
	}

	if summary == "" || !strings.Contains(summary, "tun") {
		t.Fatalf("summary %q must describe the TUN session", summary)
	}

	doc := map[string]any{}

	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("TUN document is not valid JSON: %v", err)
	}

	return doc
}

func shadowsocksForTest() config.Config {
	return config.Config{
		Type:     config.TypeShadowsocks,
		Address:  "192.0.2.10",
		Port:     8388,
		Method:   "2022-blake3-aes-128-gcm",
		Password: "synthetic-test-password",
	}
}

// TestBuildTUNDocument pins the v0.11.3 TUN dataplane contract:
// tun inbound (auto_route + strict_route) + mixed inbound, the DNS
// module in the CURRENT sing-box server format and the loop-free
// route model.
func TestBuildTUNDocument(t *testing.T) {
	cfg := shadowsocksForTest()

	tun := TUNSettings{
		InterfaceName: "FreeIranTUN",
		IPv4Address:   "172.19.0.1/30",
		IPv6Address:   "fdfe:dcba:9876::1/126",
		RemoteDNS:     "1.1.1.1",
		StrictRoute:   true,
	}

	doc := tunDocForTest(t, cfg, tun)

	// Inbounds: exactly one tun + one mixed.
	inbounds, ok := doc["inbounds"].([]any)
	if !ok || len(inbounds) != 2 {
		t.Fatalf("inbounds = %+v, want exactly tun + mixed", doc["inbounds"])
	}

	tunInbound, _ := inbounds[0].(map[string]any)
	mixedInbound, _ := inbounds[1].(map[string]any)

	if tunInbound["type"] != "tun" || tunInbound["tag"] != "tun-in" {
		t.Fatalf("first inbound = %+v, want the tun inbound", tunInbound)
	}

	if tunInbound["interface_name"] != "FreeIranTUN" {
		t.Fatalf("interface_name = %v, want FreeIranTUN", tunInbound["interface_name"])
	}

	if tunInbound["auto_route"] != true || tunInbound["strict_route"] != true {
		t.Fatalf("tun inbound = %+v, want auto_route + strict_route ON", tunInbound)
	}

	addrs, ok := tunInbound["address"].([]any)
	if !ok || len(addrs) != 2 || addrs[0] != "172.19.0.1/30" || addrs[1] != "fdfe:dcba:9876::1/126" {
		t.Fatalf("address = %v, want the configured v4+v6 TUN addresses", tunInbound["address"])
	}

	if mixedInbound["type"] != "mixed" || mixedInbound["listen_port"] != float64(18931) {
		t.Fatalf("second inbound = %+v, want the mixed diagnostic inbound on 18931", mixedInbound)
	}

	// DNS: current server format, remote resolver over the proxy.
	dns, ok := doc["dns"].(map[string]any)
	if !ok {
		t.Fatalf("dns missing from document: %+v", doc)
	}

	servers, ok := dns["servers"].([]any)
	if !ok || len(servers) != 2 {
		t.Fatalf("dns.servers = %+v, want remote + local", dns["servers"])
	}

	remote, _ := servers[0].(map[string]any)
	if remote["type"] != "https" || remote["server"] != "1.1.1.1" || remote["detour"] != "proxy" {
		t.Fatalf("remote DNS server = %+v, want DoH 1.1.1.1 via the proxy", remote)
	}

	if dns["final"] != "dns-remote" {
		t.Fatalf("dns.final = %v, want dns-remote", dns["final"])
	}

	// Route: LOOP PREVENTION is the critical invariant.
	route, ok := doc["route"].(map[string]any)
	if !ok {
		t.Fatalf("route missing from document: %+v", doc)
	}

	if route["auto_detect_interface"] != true {
		t.Fatal("route.auto_detect_interface must be true: without it the proxy upstream connection loops back into the TUN")
	}

	rules, ok := route["rules"].([]any)
	if !ok || len(rules) != 3 {
		t.Fatalf("route.rules = %+v, want sniff + hijack-dns + private-direct (final is a field)", route["rules"])
	}

	first, _ := rules[0].(map[string]any)
	if first["action"] != "sniff" {
		t.Fatalf("first rule = %+v, want the sniff action", first)
	}

	second, _ := rules[1].(map[string]any)
	if second["action"] != "hijack-dns" || second["protocol"] != "dns" {
		t.Fatalf("second rule = %+v, want DNS hijack", second)
	}

	third, _ := rules[2].(map[string]any)
	if third["ip_is_private"] != true || third["outbound"] != "direct" {
		t.Fatalf("third rule = %+v, want private ranges direct", third)
	}

	if route["final"] != "proxy" {
		t.Fatalf("route.final = %v, want proxy", route["final"])
	}

	resolver, ok := route["default_domain_resolver"].(map[string]any)
	if !ok || resolver["server"] != "dns-local" {
		t.Fatalf("default_domain_resolver = %v, want dns-local (bootstrap)", route["default_domain_resolver"])
	}

	// Outbounds: the proxy + plumbing set (shared with the mixed
	// document).
	outbounds, ok := doc["outbounds"].([]any)
	if !ok || len(outbounds) < 3 {
		t.Fatalf("outbounds = %+v, want proxy + direct + block", doc["outbounds"])
	}

	firstOut, _ := outbounds[0].(map[string]any)
	if firstOut["tag"] != "proxy" {
		t.Fatalf("first outbound = %+v, want the proxy outbound", firstOut)
	}
}

// TestBuildTUNDocumentIPv4OnlyAndWireGuard covers the optional IPv6
// address family and the WireGuard ENDPOINT form riding the same TUN
// document.
func TestBuildTUNDocumentIPv4OnlyAndWireGuard(t *testing.T) {
	// IPv4-only session (IPv6 candidates all collided).
	doc := tunDocForTest(t, shadowsocksForTest(), TUNSettings{
		InterfaceName: "FreeIranTUN",
		IPv4Address:   "172.19.0.5/30",
		RemoteDNS:     "1.1.1.1",
		StrictRoute:   true,
	})

	inbounds := doc["inbounds"].([]any)
	tunInbound, _ := inbounds[0].(map[string]any)

	addrs, _ := tunInbound["address"].([]any)
	if len(addrs) != 1 || addrs[0] != "172.19.0.5/30" {
		t.Fatalf("address = %v, want the single IPv4 TUN address", addrs)
	}

	// WireGuard rides the endpoint form.
	wg := config.Config{
		Type:       config.TypeWireGuard,
		Address:    "192.0.2.1",
		Port:       51820,
		PrivateKey: "eCtXsJZ27+4PbhDkHnB923tkUn2Gj59wZw5wFA75MnU=",
		PublicKey:  "Cr8hWlKvtDt7nrvf+f0brNQQzabAqrjfBvas9pmowjo=",
	}

	wgDoc := tunDocForTest(t, wg, TUNSettings{
		InterfaceName: "FreeIranTUN2",
		IPv4Address:   "172.19.0.9/30",
		RemoteDNS:     "1.1.1.1",
		StrictRoute:   true,
	})

	endpoints, ok := wgDoc["endpoints"].([]any)
	if !ok || len(endpoints) != 1 {
		t.Fatalf("endpoints = %+v, want the WireGuard endpoint", wgDoc["endpoints"])
	}
}

// TestValidateTUNSettings pins the TUN settings invariants.
func TestValidateTUNSettings(t *testing.T) {
	valid := TUNSettings{InterfaceName: "FreeIranTUN", IPv4Address: "172.19.0.1/30", RemoteDNS: "1.1.1.1"}
	if err := ValidateTUNSettings(valid); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}

	noName := valid
	noName.InterfaceName = ""
	if err := ValidateTUNSettings(noName); err == nil {
		t.Fatal("empty interface name must be rejected")
	}

	noAddr := valid
	noAddr.IPv4Address = ""
	if err := ValidateTUNSettings(noAddr); err == nil {
		t.Fatal("empty IPv4 address must be rejected")
	}

	noDNS := valid
	noDNS.RemoteDNS = ""
	if err := ValidateTUNSettings(noDNS); err == nil {
		t.Fatal("empty remote DNS must be rejected")
	}
}
