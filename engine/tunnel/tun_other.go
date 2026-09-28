//go:build !windows

// tun_other.go: non-Windows platforms do not serve TUN sessions in
// v0.11.3 (the sing-box TUN dataplane rollout is Windows-first —
// Wintun adapter semantics, elevation and route behavior are
// Windows-specific). The backend reports this honestly instead of
// half-configuring a foreign platform.
package tunnel

// tunPlatformSupported reports that this platform has no TUN
// implementation yet (Enable fails with ErrUnsupportedPlatform).
func tunPlatformSupported() bool { return false }

// tunProcessElevated is meaningless where TUN is unsupported; the
// elevation answer is never used before tunPlatformSupported passes.
func tunProcessElevated() bool { return false }
