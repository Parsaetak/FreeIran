//go:build windows

// tun_windows.go: the Windows side of the TUN backend — platform
// support and honest elevation detection (x/sys/windows TokenElevation,
// no UAC prompts, no ShellExecute "runas" escalation from product
// code: TUN is refused with ErrRequiresElevation and the user is told
// to start FreeIran elevated).
package tunnel

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// tunPlatformSupported reports that this build's platform serves TUN
// sessions (v0.11.3: Windows through the sing-box dataplane).
func tunPlatformSupported() bool { return true }

// tunProcessElevated reports whether the CURRENT process token is
// elevated. TUN requires administrator privileges (Wintun adapter
// creation + auto_route); the check runs BEFORE any system mutation.
func tunProcessElevated() bool {
	var token windows.Token

	err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY,
		&token,
	)
	if err != nil {
		return false
	}

	defer token.Close()

	var elevation uint32
	var returned uint32

	err = windows.GetTokenInformation(
		token,
		windows.TokenElevation,
		(*byte)(unsafe.Pointer(&elevation)),
		uint32(unsafe.Sizeof(elevation)),
		&returned,
	)
	if err != nil {
		return false
	}

	return elevation != 0
}
