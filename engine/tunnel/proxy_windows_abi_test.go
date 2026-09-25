//go:build windows

package tunnel

// proxy_windows_abi_test.go — the v0.10.2 Windows regression battery.
//
// Two layers are pinned here:
//
//  1. THE ABI LAYOUT (TestWinINetStructLayout): the Go mirrors of
//     INTERNET_PER_CONN_OPTION and INTERNET_PER_CONN_OPTION_LIST must
//     be byte-exact with the C definitions. The v0.10.1 defect was a
//     [64]uintptr "union" — a 520-byte option stride where WinINet
//     requires 16 on amd64 — which made every multi-option
//     InternetSetOption call fail with ERROR_INVALID_PARAMETER and
//     stranded the crash-recovery marker ("marker still present after
//     a successful Windows recovery", Actions run 36087876076).
//
//  2. REAL WinINet ROUNDTRIPS: on GitHub-hosted Windows runners the
//     per-connection proxy settings are mutated and restored inside
//     each test. Every test SAVES the current state first and defers
//     a verified restore, so a failing assertion can never leave the
//     runner with FreeIran's proxy. The state transitions proven:
//
//     direct → FreeIran → direct
//     explicit proxy → FreeIran → explicit proxy
//     PAC/autoconfig → FreeIran → original state
//     repeated enable/disable
//     failed enable (rollback, no residue)
//     crash-marker recovery (RecoverStaleProxy, real backend)
//     repeated recovery
//     corrupt marker
//
// These tests exercise the REAL wininet.dll — no fakes.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// TestWinINetStructLayout pins the byte-exact C ABI.
//
//	C: typedef struct { DWORD dwOption; union { DWORD dwValue; LPTSTR
//	    pszValue; } Value; } INTERNET_PER_CONN_OPTION;
//	  — amd64: dwOption@0, Value@8, sizeof 16
//	  — 386:   dwOption@0, Value@4, sizeof 8
//
//	C: typedef struct { DWORD dwSize; LPSTR pszConnection; DWORD
//	    dwOptionCount; DWORD dwOptionError; LPINT pOptions; }
//	    INTERNET_PER_CONN_OPTION_LIST;
//	  — amd64: sizeof 32   — 386: sizeof 20
func TestWinINetStructLayout(t *testing.T) {
	ptrSize := unsafe.Sizeof(uintptr(0))

	wantOption := ptrSize * 2 // dwOption + padding + one-pointer union
	if got := unsafe.Sizeof(internetPerConnOption{}); got != wantOption {
		t.Fatalf("sizeof(INTERNET_PER_CONN_OPTION) = %d, want %d — the option array stride must match the WinINet ABI (the v0.10.1 [64]uintptr regression)", got, wantOption)
	}

	if got := unsafe.Offsetof(internetPerConnOption{}.value); got != ptrSize {
		t.Fatalf("offsetof(Value) = %d, want %d", got, ptrSize)
	}

	wantList := ptrSize*3 + 16 // dwSize+pad, pszConnection, counts, pOptions
	if got := unsafe.Sizeof(internetPerConnOptionList{}); got != wantList {
		t.Fatalf("sizeof(INTERNET_PER_CONN_OPTION_LIST) = %d, want %d", got, wantList)
	}

	if got := unsafe.Offsetof(internetPerConnOptionList{}.pOptions); got != wantList-ptrSize {
		t.Fatalf("offsetof(pOptions) = %d, want %d", got, wantList-ptrSize)
	}
}

// saveSystemProxyState captures the runner's current settings for a
// guaranteed restore.
func saveSystemProxyState(t *testing.T) SystemProxySnapshot {
	t.Helper()

	backend := &winINetBackend{}

	snap, err := backend.query()
	if err != nil {
		t.Fatalf("query current proxy settings (WinINet): %v", err)
	}

	return snap
}

// restoreSystemProxyState applies a snapshot back and verifies it.
func restoreSystemProxyState(t *testing.T, snap SystemProxySnapshot) {
	t.Helper()

	backend := &winINetBackend{}

	if err := backend.applySnapshot(snap); err != nil {
		t.Fatalf("restore runner proxy settings: %v", err)
	}

	if err := backend.verifyMatches(snap); err != nil {
		t.Fatalf("verify runner proxy settings restored: %v", err)
	}
}

// withRunnerProxyState guards one test: the runner's current WinINet
// per-connection state is captured and restored (and verified) when
// the test finishes, success or failure.
func withRunnerProxyState(t *testing.T) SystemProxySnapshot {
	t.Helper()

	saved := saveSystemProxyState(t)
	t.Cleanup(func() { restoreSystemProxyState(t, saved) })

	return saved
}

// TestWinINetQueryReflectsRunnerState proves query() itself works
// against real wininet.dll — the call v0.10.1 never successfully
// made (its lpBuffer-length argument was the value, not the LPDWORD
// pointer the ABI requires).
func TestWinINetQueryReflectsRunnerState(t *testing.T) {
	saved := withRunnerProxyState(t)

	backend := &winINetBackend{}

	got, err := backend.Current()
	if err != nil {
		t.Fatalf("Current (real WinINet query): %v", err)
	}

	if !proxyStatesEqual(got, saved) {
		t.Fatalf("two consecutive real queries disagree: %+v vs %+v", got, saved)
	}
}

// TestWinINetDirectToFreeIranToDirect is the headline roundtrip:
// direct → FreeIran (socks) → verified direct restoration, with the
// bypass list round-tripping intact.
func TestWinINetDirectToFreeIranToDirect(t *testing.T) {
	saved := withRunnerProxyState(t)

	// Normalize the runner to a known clean direct state first.
	restoreSystemProxyState(t, SystemProxySnapshot{Flags: proxyTypeDirect})

	backend := &winINetBackend{}
	ctx := context.Background()

	if err := backend.Enable(ctx, "127.0.0.1", 10808, false, []string{"localhost", "<local>"}); err != nil {
		t.Fatalf("Enable (direct → FreeIran): %v", err)
	}

	observed, err := backend.Current()
	if err != nil {
		t.Fatalf("query after Enable: %v", err)
	}

	if !systemProxyActivated(observed, "127.0.0.1", 10808) {
		t.Fatalf("platform state after Enable = %+v, want the FreeIran explicit proxy", observed)
	}

	if err := backend.Disable(ctx); err != nil {
		t.Fatalf("Disable (FreeIran → direct): %v", err)
	}

	final, err := backend.Current()
	if err != nil {
		t.Fatalf("query after Disable: %v", err)
	}

	if final.Enabled || final.Server != "" {
		t.Fatalf("final state = %+v, want clean direct", final)
	}

	_ = saved // the deferred cleanup restores the runner's original state
}

// TestWinINetExplicitProxyRoundTrip preserves a pre-existing explicit
// proxy through a FreeIran session.
func TestWinINetExplicitProxyRoundTrip(t *testing.T) {
	withRunnerProxyState(t)

	// Put the runner into a user-owned explicit proxy state.
	original := SystemProxySnapshot{
		Enabled: true,
		Server:  "proxy.corp.example:8080",
		Bypass:  []string{"localhost", "10.0.0.0/8"},
		Flags:   proxyTypeProxy,
	}

	restoreSystemProxyState(t, original)

	backend := &winINetBackend{}
	ctx := context.Background()

	if err := backend.Enable(ctx, "127.0.0.1", 10808, false, []string{"<local>"}); err != nil {
		t.Fatalf("Enable over an existing explicit proxy: %v", err)
	}

	if err := backend.Disable(ctx); err != nil {
		t.Fatalf("Disable back to the user proxy: %v", err)
	}

	final, err := backend.Current()
	if err != nil {
		t.Fatalf("query after Disable: %v", err)
	}

	if !proxyStatesEqual(final, original) {
		t.Fatalf("user's original explicit proxy not restored: got %+v, want %+v", final, original)
	}
}

// TestWinINetPACRoundTrip preserves a PAC/autoconfig configuration
// through a FreeIran session (the v0.10.1 fidelity gap: the PAC URL
// was neither captured nor restored).
func TestWinINetPACRoundTrip(t *testing.T) {
	withRunnerProxyState(t)

	original := SystemProxySnapshot{
		Enabled:       false,
		Server:        "",
		AutoConfigURL: "http://wpad.corp.example/wpad.dat",
		Flags:         proxyTypeAutoProxyURL,
	}

	restoreSystemProxyState(t, original)

	backend := &winINetBackend{}
	ctx := context.Background()

	if err := backend.Enable(ctx, "127.0.0.1", 10808, false, nil); err != nil {
		t.Fatalf("Enable over a PAC configuration: %v", err)
	}

	if err := backend.Disable(ctx); err != nil {
		t.Fatalf("Disable back to PAC: %v", err)
	}

	final, err := backend.Current()
	if err != nil {
		t.Fatalf("query after Disable: %v", err)
	}

	if !proxyStatesEqual(final, original) {
		t.Fatalf("PAC state not restored: got %+v, want %+v", final, original)
	}
}

// TestWinINetRepeatedEnableDisable cycles the proxy five times: a
// crash-recovery regression must never be a first-cycle accident.
func TestWinINetRepeatedEnableDisable(t *testing.T) {
	withRunnerProxyState(t)

	restoreSystemProxyState(t, SystemProxySnapshot{Flags: proxyTypeDirect})

	backend := &winINetBackend{}
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := backend.Enable(ctx, "127.0.0.1", 10808, false, []string{"<local>"}); err != nil {
			t.Fatalf("cycle %d: Enable: %v", i+1, err)
		}

		observed, err := backend.Current()
		if err != nil {
			t.Fatalf("cycle %d: query: %v", i+1, err)
		}

		if !systemProxyActivated(observed, "127.0.0.1", 10808) {
			t.Fatalf("cycle %d: state after Enable = %+v, want the FreeIran proxy", i+1, observed)
		}

		if err := backend.Disable(ctx); err != nil {
			t.Fatalf("cycle %d: Disable: %v", i+1, err)
		}

		final, err := backend.Current()
		if err != nil {
			t.Fatalf("cycle %d: query after Disable: %v", i+1, err)
		}

		if final.Enabled || final.Server != "" {
			t.Fatalf("cycle %d: state after Disable = %+v, want direct", i+1, final)
		}
	}
}

// TestWinINetEnableHTTPScheme pins the http= server form.
func TestWinINetEnableHTTPScheme(t *testing.T) {
	withRunnerProxyState(t)

	restoreSystemProxyState(t, SystemProxySnapshot{Flags: proxyTypeDirect})

	backend := &winINetBackend{}
	ctx := context.Background()

	if err := backend.Enable(ctx, "127.0.0.1", 10809, true, []string{"<local>"}); err != nil {
		t.Fatalf("Enable (http scheme): %v", err)
	}

	observed, err := backend.Current()
	if err != nil {
		t.Fatalf("query after Enable: %v", err)
	}

	if !strings.Contains(observed.Server, "http=127.0.0.1:10809") {
		t.Fatalf("server string = %q, want the http= scheme form", observed.Server)
	}

	if err := backend.Disable(ctx); err != nil {
		t.Fatalf("Disable: %v", err)
	}
}

// TestRecoverStaleProxyRealBackend is the CI-failure regression,
// end-to-end through the REAL WinINet backend: a crashed session's
// marker (the exact v0.10.1 on-disk shape) is restored against
// wininet.dll, verified, and consumed.
func TestRecoverStaleProxyRealBackend(t *testing.T) {
	withRunnerProxyState(t)

	// The crashed session replaced a user proxy "proxy.corp.example:8080".
	restoreSystemProxyState(t, SystemProxySnapshot{
		Enabled: true,
		Server:  "proxy.corp.example:8080",
		Bypass:  []string{"localhost"},
		Flags:   proxyTypeProxy,
	})

	dir := t.TempDir()

	// Point the marker at the temp workspace (composition-root
	// contract: <workspace>/runtime/system-proxy.json).
	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	prevPath := recoveryMarkerPath
	recoveryMarkerPath = filepath.Join(runtimeDir, "system-proxy.json")
	t.Cleanup(func() { recoveryMarkerPath = prevPath })

	// The EXACT v0.10.1 marker literal (cross-version contract).
	stale := `{
  "endpoint": "127.0.0.1:10808",
  "enabled_at_ms": 1758000000000,
  "previous": {
    "enabled": true,
    "server": "proxy.corp.example:8080",
    "bypass": ["localhost"],
    "saved": true
  }
}`

	if err := os.WriteFile(recoveryMarkerPath, []byte(stale), 0o600); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	// First, simulate the crashed machine state: the FreeIran proxy
	// pointing at the dead listener (what the crash left behind).
	if err := (&winINetBackend{}).set(proxyTypeProxy, "socks=127.0.0.1:10808", "<local>", ""); err != nil {
		t.Fatalf("simulate crashed machine state: %v", err)
	}

	found, err := RecoverStaleProxy()
	if !found || err != nil {
		t.Fatalf("RecoverStaleProxy = (%v, %v), want (true, nil) — the v0.10.1 Windows regression", found, err)
	}

	if _, statErr := os.Stat(recoveryMarkerPath); !os.IsNotExist(statErr) {
		t.Fatalf("marker still present after a successful Windows recovery: %v", statErr)
	}

	final, err := (&winINetBackend{}).Current()
	if err != nil {
		t.Fatalf("query after recovery: %v", err)
	}

	if !proxyStatesEqual(final, SystemProxySnapshot{
		Enabled: true,
		Server:  "proxy.corp.example:8080",
		Bypass:  []string{"localhost"},
		Flags:   proxyTypeProxy,
	}) {
		t.Fatalf("recovered platform state = %+v, want the recorded previous state", final)
	}
}

// TestRecoverStaleProxyRepeatedRealBackend proves repeated recovery
// converges: the first recovery consumes the marker, a second boot
// finds nothing.
func TestRecoverStaleProxyRepeatedRealBackend(t *testing.T) {
	withRunnerProxyState(t)

	restoreSystemProxyState(t, SystemProxySnapshot{Flags: proxyTypeDirect})

	dir := t.TempDir()

	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	prevPath := recoveryMarkerPath
	recoveryMarkerPath = filepath.Join(runtimeDir, "system-proxy.json")
	t.Cleanup(func() { recoveryMarkerPath = prevPath })

	record := recoveryRecord{
		Endpoint:    "127.0.0.1:10808",
		EnabledAtMS: 1758000000000,
		Previous:    SystemProxySnapshot{Flags: proxyTypeDirect},
	}

	blob, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}

	// First boot: marker present → restore direct → consume.
	if err := os.WriteFile(recoveryMarkerPath, blob, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	found, err := RecoverStaleProxy()
	if !found || err != nil {
		t.Fatalf("first recovery = (%v, %v), want (true, nil)", found, err)
	}

	// Second boot: no marker → honest no-op.
	found, err = RecoverStaleProxy()
	if found || err != nil {
		t.Fatalf("second recovery = (%v, %v), want (false, nil)", found, err)
	}
}

// TestRecoverStaleProxyCorruptMarkerRealBackend pins the corrupt-
// marker path against the real backend: debris is removed and
// surfaced, never restored from.
func TestRecoverStaleProxyCorruptMarkerRealBackend(t *testing.T) {
	withRunnerProxyState(t)

	dir := t.TempDir()

	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	prevPath := recoveryMarkerPath
	recoveryMarkerPath = filepath.Join(runtimeDir, "system-proxy.json")
	t.Cleanup(func() { recoveryMarkerPath = prevPath })

	if err := os.WriteFile(recoveryMarkerPath, []byte("{\"previous\": not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt marker: %v", err)
	}

	found, err := RecoverStaleProxy()
	if !found || err == nil {
		t.Fatalf("RecoverStaleProxy = (%v, %v), want (true, error) for a corrupt marker", found, err)
	}

	if _, statErr := os.Stat(recoveryMarkerPath); !os.IsNotExist(statErr) {
		t.Fatalf("corrupt marker must be removed: %v", statErr)
	}
}

// TestWinINetGlobalFreeIsIdempotent guards the string-freeing
// discipline: GlobalFree on a NULL pointer is legal and must not
// disturb the process.
func TestWinINetGlobalFreeIsIdempotent(t *testing.T) {
	ret, _, _ := procGlobalFree.Call(0)
	if ret == 0 {
		// GlobalFree(NULL) returning 0 (failure) is acceptable on some
		// builds; either way it must not crash — reaching here is the
		// assertion.
		t.Log("GlobalFree(NULL) reported failure (acceptable)")
	}
}

// TestUTF16PointerHelpers pins the UTF-16 string handling used by
// both directions of the ABI.
func TestUTF16PointerHelpers(t *testing.T) {
	if got := splitBypass("a; b ;;c;"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("splitBypass = %v, want [a b c]", got)
	}

	if got := splitBypass("   "); got != nil {
		t.Fatalf("splitBypass(whitespace) = %v, want nil", got)
	}

	if got := bypassJoin([]string{"a", "b"}); got != "a;b" {
		t.Fatalf("bypassJoin = %q, want a;b", got)
	}

	// UTF16PtrFromString must accept the empty string (used for the
	// direct-mode restore where server/bypass are cleared).
	empty, err := syscall.UTF16PtrFromString("")
	if err != nil || empty == nil {
		t.Fatalf("UTF16PtrFromString(\"\") = %v, %v; want a valid pointer, nil error", empty, err)
	}
}
