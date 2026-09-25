package tunnel

// proxy_algebra_test.go — platform-neutral tests for the v0.10.2
// snapshot algebra: normalized comparison, mode derivation (including
// legacy v1 snapshots) and the activation predicate. These run on
// every platform CI covers; the WinINet syscall layer is exercised
// separately on Windows (proxy_windows_abi_test.go).

import "testing"

func TestProxyModeOf(t *testing.T) {
	cases := []struct {
		name string
		snap SystemProxySnapshot
		want string
	}{
		{"explicit flags", SystemProxySnapshot{Flags: proxyTypeProxy}, "explicit"},
		{"autoconfig flags", SystemProxySnapshot{Flags: proxyTypeAutoProxyURL}, "autoconfig"},
		{"autodetect flags", SystemProxySnapshot{Flags: proxyTypeAutoDetect}, "autodetect"},
		{"direct flags", SystemProxySnapshot{Flags: proxyTypeDirect}, "direct"},
		{"legacy explicit", SystemProxySnapshot{Enabled: true, Server: "p:1"}, "explicit"},
		{"legacy direct", SystemProxySnapshot{}, "direct"},
		{"pac precedence over explicit", SystemProxySnapshot{Flags: proxyTypeAutoProxyURL | proxyTypeProxy}, "autoconfig"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyModeOf(tc.snap); got != tc.want {
				t.Fatalf("proxyModeOf(%+v) = %q, want %q", tc.snap, got, tc.want)
			}
		})
	}
}

func TestProxyStatesEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b SystemProxySnapshot
		want bool
	}{
		{
			"both direct",
			SystemProxySnapshot{}, SystemProxySnapshot{Flags: proxyTypeDirect},
			true,
		},
		{
			"legacy explicit equals flags explicit",
			SystemProxySnapshot{Enabled: true, Server: "proxy:8080"},
			SystemProxySnapshot{Enabled: true, Server: "proxy:8080", Flags: proxyTypeProxy},
			true,
		},
		{
			"bypass order-insensitive",
			SystemProxySnapshot{Enabled: true, Server: "p:1", Bypass: []string{"a", "b"}},
			SystemProxySnapshot{Enabled: true, Server: "p:1", Flags: proxyTypeProxy, Bypass: []string{"b", "a"}},
			true,
		},
		{
			"server differs",
			SystemProxySnapshot{Enabled: true, Server: "p:1"},
			SystemProxySnapshot{Enabled: true, Server: "p:2"},
			false,
		},
		{
			"mode differs (explicit vs direct)",
			SystemProxySnapshot{Enabled: true, Server: "p:1"},
			SystemProxySnapshot{},
			false,
		},
		{
			"pac url differs",
			SystemProxySnapshot{Flags: proxyTypeAutoProxyURL, AutoConfigURL: "http://a/wpad.dat"},
			SystemProxySnapshot{Flags: proxyTypeAutoProxyURL, AutoConfigURL: "http://b/wpad.dat"},
			false,
		},
		{
			"pac url equal",
			SystemProxySnapshot{Flags: proxyTypeAutoProxyURL, AutoConfigURL: "http://a/wpad.dat"},
			SystemProxySnapshot{Flags: proxyTypeAutoProxyURL, AutoConfigURL: " http://a/wpad.dat "},
			true,
		},
		{
			"autodetect differs",
			SystemProxySnapshot{Flags: proxyTypeAutoDetect},
			SystemProxySnapshot{Flags: proxyTypeDirect},
			false,
		},
		{
			"bypass set size differs",
			SystemProxySnapshot{Enabled: true, Server: "p:1", Bypass: []string{"a"}},
			SystemProxySnapshot{Enabled: true, Server: "p:1", Bypass: []string{"a", "b"}},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyStatesEqual(tc.a, tc.b); got != tc.want {
				t.Fatalf("proxyStatesEqual(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSystemProxyActivated(t *testing.T) {
	cases := []struct {
		name     string
		observed SystemProxySnapshot
		host     string
		port     int
		want     bool
	}{
		{
			"socks scheme prefix",
			SystemProxySnapshot{Enabled: true, Server: "socks=127.0.0.1:10808", Flags: proxyTypeProxy},
			"127.0.0.1", 10808, true,
		},
		{
			"http scheme prefix",
			SystemProxySnapshot{Enabled: true, Server: "http=127.0.0.1:10808", Flags: proxyTypeProxy},
			"127.0.0.1", 10808, true,
		},
		{
			"plain server",
			SystemProxySnapshot{Enabled: true, Server: "127.0.0.1:10808"},
			"127.0.0.1", 10808, true,
		},
		{
			"multi-scheme list",
			SystemProxySnapshot{Enabled: true, Server: "http=127.0.0.1:10808;https=127.0.0.1:10808", Flags: proxyTypeProxy},
			"127.0.0.1", 10808, true,
		},
		{
			"flags without proxy bit",
			SystemProxySnapshot{Enabled: true, Server: "127.0.0.1:10808", Flags: proxyTypeDirect},
			"127.0.0.1", 10808, false,
		},
		{
			"explicit flag bit zero legacy not enabled",
			SystemProxySnapshot{Server: "127.0.0.1:10808"},
			"127.0.0.1", 10808, false,
		},
		{
			"different endpoint",
			SystemProxySnapshot{Enabled: true, Server: "socks=127.0.0.1:10808", Flags: proxyTypeProxy},
			"127.0.0.1", 9999, false,
		},
		{
			"partial endpoint match rejected",
			SystemProxySnapshot{Enabled: true, Server: "socks=127.0.0.1:108080", Flags: proxyTypeProxy},
			"127.0.0.1", 10808, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := systemProxyActivated(tc.observed, tc.host, tc.port); got != tc.want {
				t.Fatalf("systemProxyActivated(%+v, %s, %d) = %v, want %v",
					tc.observed, tc.host, tc.port, got, tc.want)
			}
		})
	}
}

func TestValidateRecoveryRecord(t *testing.T) {
	valid := recoveryRecord{
		SchemaVersion: 2,
		Phase:         phaseActive,
		Endpoint:      "127.0.0.1:10808",
		EnabledAtMS:   1,
		Previous: SystemProxySnapshot{
			Enabled: true,
			Server:  "proxy.corp.example:8080",
			Bypass:  []string{"localhost"},
		},
	}

	if err := validateRecoveryRecord(valid); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}

	// Legacy shape (no version, no phase) is valid.
	legacy := valid
	legacy.SchemaVersion = 0
	legacy.Phase = ""
	if err := validateRecoveryRecord(legacy); err != nil {
		t.Fatalf("legacy record rejected: %v", err)
	}

	invalid := []recoveryRecord{
		{SchemaVersion: 3},
		{SchemaVersion: 1, Phase: ownershipPhase("weird")},
		{SchemaVersion: 1, Endpoint: "bad\x00nul"},
		{SchemaVersion: 1, Previous: SystemProxySnapshot{Server: "a\x1fb"}},
		{SchemaVersion: 1, Previous: SystemProxySnapshot{AutoConfigURL: "http://x/\x07"}},
		{SchemaVersion: 1, Previous: SystemProxySnapshot{Bypass: []string{"ok", "bad\n"}}},
		{SchemaVersion: 1, Endpoint: string(make([]byte, 300))},
	}

	for i, record := range invalid {
		if err := validateRecoveryRecord(record); err == nil {
			t.Fatalf("invalid record %d accepted: %+v", i, record)
		}
	}
}
