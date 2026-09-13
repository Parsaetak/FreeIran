//go:build windows

package tunnel

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
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
// and a Value union.
//
// We use these option tags:
//
//	INTERNET_PER_CONN_FLAGS         (1)   — proxy enable + type
//	INTERNET_PER_CONN_PROXY_SERVER  (2)   — proxy server string
//	INTERNET_PER_CONN_PROXY_BYPASS  (3)   — bypass list
//	INTERNET_PER_CONN_AUTOCONFIG_URL (4)  — PAC URL (we clear it)
//
// Proxy flags:
//
//	PROXY_TYPE_DIRECT         (1) — direct connection, no proxy
//	PROXY_TYPE_PROXY         (2) — use the proxy server
//	PROXY_TYPE_AUTO_PROXY_URL (4) — use a PAC URL
//
// After setting, we call InternetSetOption with
// INTERNET_OPTION_SETTINGS_CHANGED and INTERNET_OPTION_REFRESH so
// every running app picks up the new proxy without a restart.
type winINetBackend struct {
	saved    SystemProxySnapshot
	hasSaved bool
}

// WinINet function imports.
var (
	wininet                 = syscall.NewLazyDLL("wininet.dll")
	procInternetQueryOption = wininet.NewProc("InternetQueryOptionW")
	procInternetSetOption   = wininet.NewProc("InternetSetOptionW")
)

// WinINet constants.
const (
	internetOptionPerConnectionOption = 75
	internetOptionSettingsChanged     = 39
	internetOptionRefresh             = 37

	internetPerConnFlags         = 1
	internetPerConnProxyServer   = 2
	internetPerConnProxyBypass   = 3
	internetPerConnAutoconfigURL = 4

	proxyTypeDirect       = 1
	proxyTypeProxy        = 2
	proxyTypeAutoProxyURL = 4
)

// internetPerConnOption is one entry in the per-connection list.
type internetPerConnOption struct {
	dwOption uint32
	value    [64]uintptr // union: dwValue, pszValue; sized to fit
}

// internetPerConnOptionList is the list passed to WinINet.
type internetPerConnOptionList struct {
	dwSize        uint32
	pszConnection uintptr // NULL for the default connection
	dwOptionCount uint32
	dwOptionError uint32
	pOptions      uintptr // pointer to first option
}

// Enable saves the previous proxy settings, then sets the new proxy.
// The proxy server string is built as "socks=host:port" or
// "http=host:port" depending on asHTTP. WinINet understands both
// forms in the same INTERNET_PER_CONN_PROXY_SERVER value.
func (b *winINetBackend) Enable(ctx context.Context, host string, port int, asHTTP bool, bypass []string) error {
	// 1. Save current settings.
	snap, err := b.query()
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "system_proxy", "query current settings")
	}
	b.saved = snap
	b.hasSaved = true

	// 2. Build the proxy server string.
	var scheme string
	if asHTTP {
		scheme = "http"
	} else {
		scheme = "socks"
	}
	proxyServer := fmt.Sprintf("%s=%s:%d", scheme, host, port)
	bypassStr := strings.Join(bypass, ";")

	// 3. Set the proxy.
	if err := b.set(proxyTypeProxy, proxyServer, bypassStr); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "system_proxy", "set proxy %s", proxyServer)
	}

	// 4. Notify the system so running apps pick up the change.
	if err := b.notifyChanged(); err != nil {
		// Non-fatal: the proxy is set, but apps may need a restart.
		_ = err
	}

	return nil
}

// Disable restores the previous proxy settings.
func (b *winINetBackend) Disable(ctx context.Context) error {
	if !b.hasSaved {
		// No saved state: set to direct.
		return b.set(proxyTypeDirect, "", "")
	}

	prev := b.saved
	flags := uint32(proxyTypeDirect)
	if prev.Enabled && prev.Server != "" {
		flags = proxyTypeProxy
	}
	if err := b.set(flags, prev.Server, strings.Join(prev.Bypass, ";")); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "system_proxy", "restore previous settings")
	}
	if err := b.notifyChanged(); err != nil {
		_ = err
	}

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

// query reads the current per-connection proxy settings.
func (b *winINetBackend) query() (SystemProxySnapshot, error) {
	// Allocate options for FLAGS + PROXY_SERVER + PROXY_BYPASS.
	options := make([]internetPerConnOption, 3)
	options[0].dwOption = internetPerConnFlags
	options[1].dwOption = internetPerConnProxyServer
	options[2].dwOption = internetPerConnProxyBypass

	// Buffers for the strings.
	proxyBuf := make([]uint16, 1024)
	bypassBuf := make([]uint16, 1024)

	options[1].value[0] = uintptr(unsafe.Pointer(&proxyBuf[0]))
	options[1].value[1] = uintptr(len(proxyBuf))
	options[2].value[0] = uintptr(unsafe.Pointer(&bypassBuf[0]))
	options[2].value[1] = uintptr(len(bypassBuf))

	list := internetPerConnOptionList{
		dwSize:        uint32(unsafe.Sizeof(internetPerConnOptionList{})),
		pszConnection: 0,
		dwOptionCount: uint32(len(options)),
		pOptions:      uintptr(unsafe.Pointer(&options[0])),
	}

	ret, _, err := procInternetQueryOption.Call(
		0,
		uintptr(internetOptionPerConnectionOption),
		uintptr(unsafe.Pointer(&list)),
		uintptr(list.dwSize),
	)
	if ret == 0 {
		return SystemProxySnapshot{}, err
	}

	flags := uint32(options[0].value[0])
	enabled := (flags & proxyTypeProxy) != 0
	proxy := utf16ToString(proxyBuf)
	bypass := utf16ToString(bypassBuf)

	var bypassList []string
	if bypass != "" {
		for _, s := range strings.Split(bypass, ";") {
			s = strings.TrimSpace(s)
			if s != "" {
				bypassList = append(bypassList, s)
			}
		}
	}

	return SystemProxySnapshot{
		Enabled:  enabled,
		Server:   proxy,
		Bypass:   bypassList,
		Override: bypass,
		Saved:    b.hasSaved,
	}, nil
}

// set writes the per-connection proxy settings.
func (b *winINetBackend) set(flags uint32, proxyServer, bypass string) error {
	proxyUTF16 := syscall.StringToUTF16Ptr(proxyServer)
	bypassUTF16 := syscall.StringToUTF16Ptr(bypass)

	options := make([]internetPerConnOption, 3)
	options[0].dwOption = internetPerConnFlags
	options[0].value[0] = uintptr(flags)

	options[1].dwOption = internetPerConnProxyServer
	options[1].value[0] = uintptr(unsafe.Pointer(proxyUTF16))

	options[2].dwOption = internetPerConnProxyBypass
	options[2].value[0] = uintptr(unsafe.Pointer(bypassUTF16))

	list := internetPerConnOptionList{
		dwSize:        uint32(unsafe.Sizeof(internetPerConnOptionList{})),
		pszConnection: 0,
		dwOptionCount: uint32(len(options)),
		pOptions:      uintptr(unsafe.Pointer(&options[0])),
	}

	ret, _, err := procInternetSetOption.Call(
		0,
		uintptr(internetOptionPerConnectionOption),
		uintptr(unsafe.Pointer(&list)),
		uintptr(list.dwSize),
	)
	if ret == 0 {
		return err
	}
	return nil
}

// notifyChanged broadcasts that proxy settings changed so running
// apps refresh.
func (b *winINetBackend) notifyChanged() error {
	r, _, err := procInternetSetOption.Call(0, uintptr(internetOptionSettingsChanged), 0, 0)
	if r == 0 {
		return err
	}
	r, _, err = procInternetSetOption.Call(0, uintptr(internetOptionRefresh), 0, 0)
	if r == 0 {
		return err
	}
	return nil
}

// utf16ToString converts a UTF-16 NUL-terminated buffer to a Go string.
func utf16ToString(buf []uint16) string {
	// Find the NUL terminator.
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return syscall.UTF16ToString(buf[:n])
}

// newSystemProxyBackend returns the WinINet-backed implementation on
// Windows.
func newSystemProxyBackend() SystemProxyBackend {
	return &winINetBackend{}
}
