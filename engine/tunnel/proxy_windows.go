//go:build windows

package tunnel

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// winINetBackend implements SystemProxyBackend using WinINet's
// per-connection options. This is the Windows-native way to set the
// system proxy without writing arbitrary registry values directly.
//
// Reference:
//
//	https://learn.microsoft.com/en-us/windows/win32/wininet/setting-and-retrieving-internet-options
//
// The InternetQueryOption / InternetSetOption API with
// INTERNET_OPTION_PER_CONNECTION_OPTION operates on a
// INTERNET_PER_CONN_OPTION_LIST structure that contains a list of
// INTERNET_PER_CONN_OPTION entries. Each entry has a dwOption tag
// and a Value union (DWORD dwValue / LPTSTR pszValue).
//
// v0.10.2 ABI CORRECTIONS (root cause of the v0.10.1 Windows
// regression — Actions run 36087876076, "marker still present after a
// successful Windows recovery"):
//
//  1. INTERNET_PER_CONN_OPTION is { DWORD dwOption; union { DWORD
//     dwValue; LPTSTR pszValue; } Value; } — the union is ONE
//     pointer-sized field. v0.10.1 declared it as [64]uintptr, a
//     520-byte stride on amd64, so WinINet — which strides the array
//     by 16 bytes — read options 2..n from inside option 1's zeroed
//     padding, saw unknown dwOption values, and failed the whole call
//     with ERROR_INVALID_PARAMETER. Every Enable/Disable/Restore with
//     more than one option failed on Windows.
//  2. InternetQueryOption's lpBuffer-length parameter is LPDWORD (a
//     POINTER to the size). v0.10.1 passed the size value itself,
//     which WinINet dereferences — an access violation waiting for
//     the first query call.
//  3. Query strings: WinINet ALLOCATES the returned pszValue buffers
//     (GlobalAlloc); the caller must read them and free them with
//     GlobalFree. v0.10.1 passed caller-owned buffers and a
//     "pointer+length" pair that does not exist in the ABI.
//
// The option tags used here:
//
//	INTERNET_PER_CONN_FLAGS              (1) — proxy enable + type bits
//	INTERNET_PER_CONN_PROXY_SERVER       (2) — explicit proxy string
//	INTERNET_PER_CONN_PROXY_BYPASS       (3) — bypass list
//	INTERNET_PER_CONN_AUTOCONFIG_URL     (4) — PAC/autoconfig URL
//	INTERNET_PER_CONN_AUTODISCOVERY_FLAGS (5) — AUTO_PROXY_FLAG_* mask
//
// Proxy flags (INTERNET_PER_CONN_FLAGS):
//
//	PROXY_TYPE_DIRECT          (1) — direct connection, no proxy
//	PROXY_TYPE_PROXY           (2) — explicit proxy server
//	PROXY_TYPE_AUTO_PROXY_URL  (4) — PAC/autoconfiguration
//	PROXY_TYPE_AUTO_DETECT     (8) — WPAD automatic detection
//
// v0.10.3 fidelity correction: the AUTO_DETECT bit of
// INTERNET_PER_CONN_FLAGS is only HALF of the WPAD story. Windows
// keeps the connection's autodiscovery policy in the SEPARATE
// INTERNET_PER_CONN_AUTODISCOVERY_FLAGS option (AUTO_PROXY_FLAG_*:
// USER_SET 0x1, ALWAYS_DETECT 0x2, DETECTION_RUN 0x4, …) and
// re-derives the AUTO_DETECT flag bit from it on query. v0.10.2
// neither captured nor restored option 5, so a session that saved a
// connection with WPAD enabled could not restore it faithfully and
// restore VERIFICATION could observe an AUTO_DETECT bit the record
// never described. v0.10.3 captures option 5 tolerantly (it is
// rejected by some builds when no autodiscovery policy exists),
// persists it in the snapshot, and applies it back when the record
// carries it. Zero continues to mean "not captured" everywhere.
//
// String allocation/free semantics: set() hands WinINet pointers to
// UTF-16 buffers owned by this call (kept alive across the syscall);
// query() reads WinINet-owned pointers and releases each with
// GlobalFree after conversion. Win32 BOOL semantics: a zero return
// means failure and GetLastError carries the reason (surfaced as
// syscall.Errno through the Call error slot).
//
// Scope contract (documented narrowing): all operations target the
// LAN/default connection (pszConnection = NULL). RAS/dial-up per-
// connection profiles are NOT captured or restored by this backend.
//
// After setting, we call InternetSetOption with
// INTERNET_OPTION_SETTINGS_CHANGED and INTERNET_OPTION_REFRESH so
// every running app picks up the new proxy without a restart.
type winINetBackend struct {
	saved    SystemProxySnapshot
	hasSaved bool
}

// WinINet / kernel32 function imports.
var (
	wininet                 = syscall.NewLazyDLL("wininet.dll")
	procInternetQueryOption = wininet.NewProc("InternetQueryOptionW")
	procInternetSetOption   = wininet.NewProc("InternetSetOptionW")

	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procGlobalFree = kernel32.NewProc("GlobalFree")
)

const (
	internetOptionPerConnectionOption = 75
	internetOptionSettingsChanged     = 39
	internetOptionRefresh             = 37

	// maxQueryStrings caps the NUL scan of WinINet-allocated strings
	// (registry-backed proxy strings are far below this).
	maxQueryStrings = 1 << 15
)

// internetPerConnOption is one entry in the per-connection list.
// The union is ONE pointer-sized field: Go's natural layout for
// { uint32; uintptr } matches the C layout on amd64 (16 bytes) and
// 386 (8 bytes) exactly — asserted by the ABI layout test on Windows.
type internetPerConnOption struct {
	dwOption uint32
	value    uintptr // union: dwValue (DWORD) or pszValue (LPTSTR)
}

// internetPerConnOptionList is the list passed to WinINet.
type internetPerConnOptionList struct {
	dwSize        uint32
	pszConnection uintptr // NULL for the default (LAN) connection
	dwOptionCount uint32
	dwOptionError uint32
	pOptions      uintptr // pointer to first option
}

// Enable saves the previous proxy settings, then sets the new proxy.
// The proxy server string is built as "socks=host:port" or
// "http=host:port" depending on asHTTP. WinINet understands both
// forms in the same INTERNET_PER_CONN_PROXY_SERVER value.
func (b *winINetBackend) Enable(ctx context.Context, host string, port int, asHTTP bool, bypass []string) error {
	// 1. Save current settings (the ACTUAL platform state).
	snap, err := b.query()
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "system_proxy", "query current settings")
	}
	b.saved = snap
	b.hasSaved = true

	// 2. Build the proxy server string.
	scheme := "socks"
	if asHTTP {
		scheme = "http"
	}
	proxyServer := fmt.Sprintf("%s=%s:%d", scheme, host, port)
	bypassStr := strings.Join(bypass, ";")

	// 3. Set the explicit proxy (clears PAC/autodetect bits for the
	// session — the previous state is saved above and restored by
	// Disable/Restore).
	if err := b.set(proxyTypeProxy, proxyServer, bypassStr, ""); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "system_proxy", "set proxy %s", proxyServer)
	}

	// 4. Notify the system so running apps pick up the change. The
	// hard correctness gate is the platform mutation + verification
	// below; a notification failure is non-fatal but must be
	// visible in diagnostics (v0.10.4), never silently discarded.
	b.notifyChangedReported("activate")

	// 5. Verify the platform state actually shows the new proxy
	// (v0.10.2 transactional activation — in-memory success is not
	// evidence).
	observed, err := b.query()
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "system_proxy", "verify proxy activation")
	}

	if !systemProxyActivated(observed, host, port) {
		return firerrors.New(firerrors.KindEnvironment, Subsystem, "system_proxy",
			"proxy activation verification failed: WinINet does not report the activated proxy")
	}

	return nil
}

// Disable restores the previous proxy settings.
func (b *winINetBackend) Disable(ctx context.Context) error {
	if !b.hasSaved {
		// No saved state: fall back to direct. Ownership semantics
		// (the Controller only calls Disable while owning the proxy)
		// make this the honest last resort.
		if err := b.set(proxyTypeDirect, "", "", ""); err != nil {
			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "system_proxy", "reset to direct")
		}

		b.notifyChangedReported("reset-to-direct")
		return nil
	}

	prev := b.saved

	if err := b.applySnapshot(prev); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "system_proxy", "restore previous settings")
	}

	// v0.10.2: verify the restoration against the ACTUAL platform
	// state before reporting success.
	if err := b.verifyMatches(prev); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "system_proxy", "verify previous settings restored")
	}

	b.notifyChangedReported("restore-previous")

	b.hasSaved = false
	b.saved = SystemProxySnapshot{}
	return nil
}

// Snapshot returns the saved (previous) state, or the current state
// if no Enable has been called yet.
func (b *winINetBackend) Snapshot() SystemProxySnapshot {
	if b.hasSaved {
		return b.saved
	}
	snap, _ := b.query()
	return snap
}

// Current reads the ACTUAL WinINet per-connection state right now
// (v0.10.2 transactional ownership contract).
func (b *winINetBackend) Current() (SystemProxySnapshot, error) {
	return b.query()
}

// Restore applies a previously persisted proxy state (crash
// recovery — see recovery.go). It applies the recorded state with the
// same fidelity Enable captured (mode flags, explicit server, bypass,
// PAC URL, autodetect) and VERIFIES the platform state matches before
// reporting success — RecoverStaleProxy removes the ownership marker
// only after this verification.
func (b *winINetBackend) Restore(previous SystemProxySnapshot) error {
	if err := b.applySnapshot(previous); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "recovery", "restore recorded system-proxy state")
	}

	if err := b.verifyMatches(previous); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "recovery", "verify recorded system-proxy restoration")
	}

	b.notifyChangedReported("recovery-restore")

	return nil
}

// applySnapshot writes a snapshot back to WinINet, preserving the
// captured proxy mode (explicit / PAC / autodetect / direct) and the
// associated strings. Legacy (v1) snapshots — no Flags captured —
// derive the mode from Enabled/Server (documented v1 fidelity limit).
//
// v0.10.3: a snapshot that captured the connection's
// INTERNET_PER_CONN_AUTODISCOVERY_FLAGS (v2+ records) also restores
// it, so WPAD autodetection state is faithful on both directions of
// a save/restore cycle. A record without it (zero) leaves the
// connection's autodiscovery policy untouched — "not captured" must
// never become "clobbered".
func (b *winINetBackend) applySnapshot(s SystemProxySnapshot) error {
	flags := s.Flags & allProxyTypeFlags

	if flags == 0 {
		// Legacy derivation (v0.10.1 records and fresh snapshots).
		if s.Enabled && s.Server != "" {
			flags = proxyTypeProxy
		} else {
			flags = proxyTypeDirect
		}
	}

	// PAC flag without a recorded URL cannot be faithfully applied —
	// drop the bit rather than leaving WinINet pointing at a missing
	// autoconfig URL (the v1 fidelity limit, logged by the caller).
	pac := s.AutoConfigURL
	if pac == "" {
		flags &^= proxyTypeAutoProxyURL
	}

	// A plain-direct restore clears everything else; WinINet expects
	// the non-direct flags to carry their strings.
	if flags == proxyTypeDirect {
		return b.setFull(flags, "", bypassJoin(s.Bypass), "", s.AutoDiscoveryFlags)
	}

	return b.setFull(flags, s.Server, bypassJoin(s.Bypass), pac, s.AutoDiscoveryFlags)
}

// verifyMatches reads the ACTUAL platform state and compares it
// (normalized) against the expected snapshot.
func (b *winINetBackend) verifyMatches(expected SystemProxySnapshot) error {
	observed, err := b.query()
	if err != nil {
		return fmt.Errorf("read back platform state: %w", err)
	}

	if !proxyStatesEqual(observed, expected) {
		return fmt.Errorf("platform state after apply (mode %q, server %q) does not match the expected state (mode %q, server %q)",
			proxyModeOf(observed), observed.Server,
			proxyModeOf(expected), expected.Server)
	}

	return nil
}

// bypassJoin renders the bypass list the way WinINet stores it.
// Entries MUST already be in WinINet's documented bypass grammar
// (host names, IP literals, wildcard patterns, "<local>" — never
// CIDR notation; see Options.Bypass and TestWinINetBypassGrammar).
func bypassJoin(bypass []string) string {
	return strings.Join(bypass, ";")
}

// query reads the current per-connection proxy settings.
//
// v0.10.2 semantics: FLAGS + PROXY_SERVER + PROXY_BYPASS are queried
// together (all always valid); the AUTOCONFIG_URL is queried in a
// second, tolerant call (some Windows builds reject the option when
// no PAC is configured). v0.10.3 adds the tolerant
// AUTODISCOVERY_FLAGS query — the other half of the WPAD state.
// Returned strings are WinINet-allocated and are freed with
// GlobalFree after conversion.
func (b *winINetBackend) query() (SystemProxySnapshot, error) {
	flags, server, bypass, err := b.queryCore()
	if err != nil {
		return SystemProxySnapshot{}, err
	}

	pac := b.queryAutoconfigURL()
	autoDiscovery := b.queryAutoDiscoveryFlags()

	snapshot := SystemProxySnapshot{
		Enabled:            flags&proxyTypeProxy != 0,
		Server:             server,
		Bypass:             splitBypass(bypass),
		Override:           bypass,
		AutoConfigURL:      pac,
		AutoDetect:         flags&proxyTypeAutoDetect != 0,
		Flags:              flags & allProxyTypeFlags,
		AutoDiscoveryFlags: autoDiscovery,
		Saved:              b.hasSaved,
	}

	return snapshot, nil
}

// queryCore queries FLAGS + PROXY_SERVER + PROXY_BYPASS in one call.
//
// v0.10.4: the documented Windows 7+ query order is used — the
// connection's flags are queried through INTERNET_PER_CONN_FLAGS_UI
// first (the UI-visible state), and only if that query FAILS does the
// call fall back to the legacy INTERNET_PER_CONN_FLAGS tag. Setting
// keeps using INTERNET_PER_CONN_FLAGS (setFull), as documented. Both
// paths return the same PROXY_TYPE_* bits for every state the
// snapshot algebra models; the fallback keeps pre-7 builds working.
func (b *winINetBackend) queryCore() (flags uint32, server, bypass string, err error) {
	flags, server, bypass, err = b.queryCoreWithTag(internetPerConnFlagsUI)
	if err != nil {
		flags, server, bypass, err = b.queryCoreWithTag(internetPerConnFlags)
	}

	return flags, server, bypass, err
}

// queryCoreWithTag is queryCore for one specific flags option tag.
func (b *winINetBackend) queryCoreWithTag(flagOption uint32) (flags uint32, server, bypass string, err error) {
	options := make([]internetPerConnOption, 3)
	options[0].dwOption = flagOption
	options[1].dwOption = internetPerConnProxyServer
	options[2].dwOption = internetPerConnProxyBypass

	list := internetPerConnOptionList{
		dwSize:        uint32(unsafe.Sizeof(internetPerConnOptionList{})),
		pszConnection: 0,
		dwOptionCount: uint32(len(options)),
		pOptions:      uintptr(unsafe.Pointer(&options[0])),
	}

	// InternetQueryOption's last parameter is LPDWORD: a pointer to
	// the buffer size, updated with the required size on return.
	size := list.dwSize

	ret, _, callErr := procInternetQueryOption.Call(
		0,
		uintptr(internetOptionPerConnectionOption),
		uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Pointer(&size)),
	)
	runtime.KeepAlive(&options)
	runtime.KeepAlive(&list)

	if ret == 0 {
		return 0, "", "", win32CallError("InternetQueryOption(INTERNET_OPTION_PER_CONNECTION_OPTION)", callErr)
	}

	// The string options come back as WinINet-allocated
	// (GlobalAlloc) buffers that MUST be freed with GlobalFree.
	flags = uint32(options[0].value)
	server = globalAllocedUTF16(&options[1].value)
	bypass = globalAllocedUTF16(&options[2].value)

	return flags, server, bypass, nil
}

// queryAutoconfigURL queries the PAC/autoconfig URL tolerantly: an
// error (option rejected, no PAC configured) simply means "empty".
func (b *winINetBackend) queryAutoconfigURL() string {
	options := make([]internetPerConnOption, 1)
	options[0].dwOption = internetPerConnAutoconfigURL

	list := internetPerConnOptionList{
		dwSize:        uint32(unsafe.Sizeof(internetPerConnOptionList{})),
		pszConnection: 0,
		dwOptionCount: uint32(len(options)),
		pOptions:      uintptr(unsafe.Pointer(&options[0])),
	}

	size := list.dwSize

	ret, _, _ := procInternetQueryOption.Call(
		0,
		uintptr(internetOptionPerConnectionOption),
		uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Pointer(&size)),
	)
	runtime.KeepAlive(&options)
	runtime.KeepAlive(&list)

	if ret == 0 {
		return ""
	}

	return globalAllocedUTF16(&options[0].value)
}

// queryAutoDiscoveryFlags queries the connection's
// INTERNET_PER_CONN_AUTODISCOVERY_FLAGS (the AUTO_PROXY_FLAG_* mask
// that accompanies the AUTO_DETECT flag bit). Tolerant like the PAC
// query: an error or a zero value means "not reported by this
// build/connection", which the snapshot algebra treats as "not
// captured" — never as a policy of "none".
func (b *winINetBackend) queryAutoDiscoveryFlags() uint32 {
	options := make([]internetPerConnOption, 1)
	options[0].dwOption = internetPerConnAutoDiscoveryFlags

	list := internetPerConnOptionList{
		dwSize:        uint32(unsafe.Sizeof(internetPerConnOptionList{})),
		pszConnection: 0,
		dwOptionCount: uint32(len(options)),
		pOptions:      uintptr(unsafe.Pointer(&options[0])),
	}

	size := list.dwSize

	ret, _, _ := procInternetQueryOption.Call(
		0,
		uintptr(internetOptionPerConnectionOption),
		uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Pointer(&size)),
	)
	runtime.KeepAlive(&options)
	runtime.KeepAlive(&list)

	if ret == 0 {
		return 0
	}

	return uint32(options[0].value)
}

// set writes the per-connection proxy settings. Empty autoconfig
// selects a 3-option list; a non-empty PAC URL is appended as its
// own INTERNET_PER_CONN_AUTOCONFIG_URL entry.
func (b *winINetBackend) set(flags uint32, proxyServer, bypass, autoconfig string) error {
	return b.setFull(flags, proxyServer, bypass, autoconfig, 0)
}

// setFull is set with autodiscovery-flag fidelity: a non-zero
// autoDiscovery value appends an INTERNET_PER_CONN_AUTODISCOVERY_FLAGS
// entry so the connection's WPAD policy is written in the SAME
// transaction as the flags (WinINet applies a per-connection list
// atomically — flags and autodiscovery must never be two calls).
func (b *winINetBackend) setFull(flags uint32, proxyServer, bypass, autoconfig string, autoDiscovery uint32) error {
	serverUTF16, err := syscall.UTF16PtrFromString(proxyServer)
	if err != nil {
		return fmt.Errorf("proxy server string: %w", err)
	}

	bypassUTF16, err := syscall.UTF16PtrFromString(bypass)
	if err != nil {
		return fmt.Errorf("bypass string: %w", err)
	}

	count := 3
	var pacUTF16 *uint16

	if autoconfig != "" {
		pacUTF16, err = syscall.UTF16PtrFromString(autoconfig)
		if err != nil {
			return fmt.Errorf("autoconfig URL: %w", err)
		}

		count = 4
	}

	if autoDiscovery != 0 {
		count++
	}

	options := make([]internetPerConnOption, count)
	options[0].dwOption = internetPerConnFlags
	options[0].value = uintptr(flags)

	options[1].dwOption = internetPerConnProxyServer
	options[1].value = uintptr(unsafe.Pointer(serverUTF16))

	options[2].dwOption = internetPerConnProxyBypass
	options[2].value = uintptr(unsafe.Pointer(bypassUTF16))

	next := 3

	if pacUTF16 != nil {
		options[next].dwOption = internetPerConnAutoconfigURL
		options[next].value = uintptr(unsafe.Pointer(pacUTF16))
		next++
	}

	if autoDiscovery != 0 {
		options[next].dwOption = internetPerConnAutoDiscoveryFlags
		options[next].value = uintptr(autoDiscovery)
	}

	list := internetPerConnOptionList{
		dwSize:        uint32(unsafe.Sizeof(internetPerConnOptionList{})),
		pszConnection: 0,
		dwOptionCount: uint32(len(options)),
		pOptions:      uintptr(unsafe.Pointer(&options[0])),
	}

	ret, _, callErr := procInternetSetOption.Call(
		0,
		uintptr(internetOptionPerConnectionOption),
		uintptr(unsafe.Pointer(&list)),
		uintptr(list.dwSize),
	)
	runtime.KeepAlive(&options)
	runtime.KeepAlive(&list)
	runtime.KeepAlive(serverUTF16)
	runtime.KeepAlive(bypassUTF16)
	runtime.KeepAlive(pacUTF16)

	if ret == 0 {
		return win32CallError("InternetSetOption(INTERNET_OPTION_PER_CONNECTION_OPTION)", callErr)
	}

	return nil
}

// notifyChanged broadcasts that proxy settings changed so running
// apps refresh.
func (b *winINetBackend) notifyChanged() error {
	r, _, err := procInternetSetOption.Call(0, uintptr(internetOptionSettingsChanged), 0, 0)
	if r == 0 {
		return win32CallError("InternetSetOption(INTERNET_OPTION_SETTINGS_CHANGED)", err)
	}
	r, _, err = procInternetSetOption.Call(0, uintptr(internetOptionRefresh), 0, 0)
	if r == 0 {
		return win32CallError("InternetSetOption(INTERNET_OPTION_REFRESH)", err)
	}
	return nil
}

// notifyChangedReported runs notifyChanged and routes a failure into
// the structured diagnostics log (v0.10.4). The transactional
// contract treats platform mutation + verification as the hard
// correctness gate — a failed notification never rolls the state
// back — but it must not silently disappear either: callers and CI
// logs see exactly which stage failed and why.
func (b *winINetBackend) notifyChangedReported(stage string) {
	if err := b.notifyChanged(); err != nil {
		logging.W(Subsystem, "proxy_notify_failed",
			"system-proxy notification after %s failed: %v", stage, err)
	}
}

// win32CallError renders a Win32 syscall failure with BOTH the
// numeric code and the human-readable message (v0.10.4 CI
// diagnostics): a bare syscall.Errno prints as "The parameter is
// incorrect." — true, but the errno number (87 =
// ERROR_INVALID_PARAMETER) and the failing operation are what make
// an Actions log diagnosable.
func win32CallError(op string, callErr error) error {
	if errno, ok := callErr.(syscall.Errno); ok {
		return fmt.Errorf("%s failed: win32 error %d (%s)", op, errno, errno.Error())
	}

	return fmt.Errorf("%s failed: %w", op, callErr)
}

// globalAllocedUTF16 converts a WinINet-allocated (GlobalAlloc)
// UTF-16 string pointer to a Go string and frees the buffer. A NULL
// pointer maps to the empty string. The pointer arrives inside the
// INTERNET_PER_CONN_OPTION union (a pointer-sized uintptr field);
// it is reinterpreted through the field's storage — the value is OS
// memory from WinINet's GlobalAlloc, never Go heap, so the
// reinterpretation is sound and keeps go vet's unsafeptr analysis
// clean (no uintptr arithmetic, no uintptr→Pointer conversion).
func globalAllocedUTF16(field *uintptr) string {
	if field == nil || *field == 0 {
		return ""
	}

	base := *(*unsafe.Pointer)(unsafe.Pointer(field))
	chars := make([]uint16, 0, 64)

	for i := 0; i < maxQueryStrings; i++ {
		c := *(*uint16)(unsafe.Add(base, uintptr(i)*2))
		if c == 0 {
			break
		}
		chars = append(chars, c)
	}

	procGlobalFree.Call(*field)

	return string(utf16.Decode(chars))
}

// splitBypass splits the WinINet override string into entries.
func splitBypass(bypass string) []string {
	if strings.TrimSpace(bypass) == "" {
		return nil
	}

	var out []string

	for _, s := range strings.Split(bypass, ";") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}

	return out
}

// newSystemProxyBackend returns the WinINet-backed implementation on
// Windows.
func newSystemProxyBackend() SystemProxyBackend {
	return &winINetBackend{}
}
