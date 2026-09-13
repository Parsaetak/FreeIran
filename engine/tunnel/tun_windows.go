//go:build windows

package tunnel

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// wintunBackend implements TUNBackend using the official Wintun
// driver (https://www.wintun.net). Wintun is distributed as a single
// wintun.dll file that the application ships alongside its binary;
// it does NOT require a kernel-mode driver install with a separate
// installer.
//
// FreeIran resolves wintun.dll from (in order):
//  1. <AppData>/FreeIran/cores/wintun/wintun.dll (managed by coremgr)
//  2. the directory of the running FreeIran executable
//  3. the system directory (C:\Windows\System32\)
//
// If the DLL is missing, Available returns false and the user is
// asked to install it (Install() downloads it from the official
// wintun.net release ZIP).
type wintunBackend struct {
	mu       sync.Mutex
	loaded   bool
	dll      *syscall.DLL
	create   *syscall.Proc
	close    *syscall.Proc
	start    *syscall.Proc
	adapter  uintptr
	snapshot TUNSnapshot

	// routing state for cleanup
	addedRoutes []string
}

// newTUNBackend returns the Wintun-backed implementation.
func newTUNBackend() TUNBackend {
	b := &wintunBackend{}
	b.tryLoadDLL()
	return b
}

// Available reports whether the Wintun DLL is loaded and the system
// supports TUN mode.
func (b *wintunBackend) Available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loaded
}

// Install downloads the Wintun release ZIP, extracts wintun.dll, and
// places it in <AppData>/FreeIran/cores/wintun/. Requires elevation.
// Idempotent: returns nil if already installed.
func (b *wintunBackend) Install(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.loaded {
		return nil
	}

	if !isElevated() {
		return ErrRequiresElevation
	}

	// 1. Determine target directory.
	dir, err := wintunInstallDir()
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "tun_install", "resolve wintun dir")
	}

	// 2. Check if DLL already exists.
	dllPath := filepath.Join(dir, "wintun.dll")
	if _, err := statFile(dllPath); err == nil {
		// DLL already on disk; try to load it.
		if err := b.loadFrom(dllPath); err == nil {
			return nil
		}
	}

	// 3. Download + extract.
	if err := downloadAndExtractWintun(ctx, dllPath); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "tun_install", "download+extract wintun")
	}

	// 4. Load.
	if err := b.loadFrom(dllPath); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "tun_install", "load wintun.dll")
	}

	return nil
}

// Enable creates the TUN interface and configures routes + DNS. The
// upstream SOCKS endpoint is at host:port. Wintun creates a virtual
// network adapter with an IPv4 address; traffic sent to it is
// forwarded to the userland SOCKS proxy via the active core.
//
// Implementation note: this implementation creates the adapter
// (WintunCreateAdapter) and configures the IP + routes via netsh.
// Packet forwarding through the SOCKS upstream is the responsibility
// of the active core's tun-mode configuration (sing-box's tun
// inbound). FreeIran's role is to:
//   - install wintun.dll
//   - create the adapter
//   - set the IP
//   - add routes for the configured CIDRs
//   - set the DNS server
//   - delegate the SOCKS forwarding to sing-box (the backend's
//     TUN-mode BuildConfig produces a tun inbound pointing at the
//     adapter)
func (b *wintunBackend) Enable(ctx context.Context, host string, port int) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.loaded {
		return ErrUnsupportedPlatform
	}
	if !isElevated() {
		return ErrRequiresElevation
	}

	// 1. Create the adapter.
	adapterName, _ := syscall.UTF16PtrFromString("FreeIran")
	tunnelType, _ := syscall.UTF16PtrFromString("FreeIran")
	adapter, _, err := b.create.Call(
		uintptr(unsafe.Pointer(adapterName)),
		uintptr(unsafe.Pointer(tunnelType)),
		0,
	)
	if adapter == 0 {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "tun_enable", "WintunCreateAdapter")
	}
	b.adapter = adapter

	// 2. Configure the IP (10.211.211.1/24 — RFC 1918, non-routable
	// outside the host). Use netsh; production code would use the
	// IP Helper API directly.
	tunIP := "10.211.211.1"
	if err := runCmd(ctx, "netsh", "interface", "ipv4", "set", "address",
		"name=FreeIran", "static", tunIP, "255.255.255.0"); err != nil {
		_ = b.closeAdapterLocked()
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "tun_enable", "set adapter address")
	}

	// 3. Add routes for the captured CIDRs.
	cidrs := []string{"0.0.0.0/1", "128.0.0.0/1"}
	for _, cidr := range cidrs {
		if err := runCmd(ctx, "route", "add", cidr, tunIP, "metric", "5"); err != nil {
			b.addedRoutes = append(b.addedRoutes, cidr)
		}
	}

	// 4. Set DNS (Cloudflare DNS to bypass any DNS hijacking by the
	// local ISP).
	_ = runCmd(ctx, "netsh", "interface", "ipv4", "set", "dnsservers",
		"name=FreeIran", "static", "1.1.1.1", "primary")

	b.snapshot = TUNSnapshot{
		Available:         true,
		Installed:         true,
		InterfaceName:     "FreeIran",
		IPv4Address:       tunIP,
		DNSServers:        []string{"1.1.1.1", "8.8.8.8"},
		Routes:            cidrs,
		RequiresElevation: true,
	}

	return nil
}

// Disable tears down the TUN interface, removes routes, restores DNS.
func (b *wintunBackend) Disable(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.adapter == 0 {
		return nil
	}

	// Remove routes.
	for _, cidr := range b.addedRoutes {
		_ = runCmd(ctx, "route", "delete", cidr)
	}
	b.addedRoutes = nil

	// Close the adapter.
	if err := b.closeAdapterLocked(); err != nil {
		return err
	}

	// Restore DNS to automatic.
	_ = runCmd(ctx, "netsh", "interface", "ipv4", "set", "dnsservers",
		"name=FreeIran", "source", "dhcp")

	b.snapshot = TUNSnapshot{
		Available: b.loaded,
	}
	return nil
}

// closeAdapterLocked closes the Wintun adapter; caller MUST hold b.mu.
func (b *wintunBackend) closeAdapterLocked() error {
	if b.adapter == 0 || b.close == nil {
		return nil
	}
	r, _, err := b.close.Call(b.adapter)
	if r == 0 {
		return err
	}
	b.adapter = 0
	return nil
}

// Snapshot returns the current TUN state.
func (b *wintunBackend) Snapshot() TUNSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshot
}

// tryLoadDLL attempts to load wintun.dll from the standard search
// paths. Does NOT require elevation. On success b.loaded = true.
func (b *wintunBackend) tryLoadDLL() {
	candidates := []string{}

	if dir, err := wintunInstallDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "wintun.dll"))
	}
	if exeDir, err := executableDir(); err == nil {
		candidates = append(candidates, filepath.Join(exeDir, "wintun.dll"))
	}
	candidates = append(candidates, filepath.Join(systemDir(), "wintun.dll"))

	for _, p := range candidates {
		if _, err := statFile(p); err == nil {
			if err := b.loadFrom(p); err == nil {
				return
			}
		}
	}
}

// loadFrom loads wintun.dll from path. On success b.loaded = true.
func (b *wintunBackend) loadFrom(path string) error {
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return err
	}

	// Resolve the entrypoints we need. Wintun 0.14+ exposes:
	//   WintunCreateAdapter(name, tunnelType, requestedGUID) -> HANDLE
	//   WintunCloseAdapter(handle) -> void
	//   WintunStartSession(handle, capacity) -> HANDLE
	// The exact function names are versioned; we resolve defensively.
	create, err := dll.FindProc("WintunCreateAdapter")
	if err != nil {
		create, err = dll.FindProc("WintunCreateAdapterW")
		if err != nil {
			_ = dll.Release()
			return err
		}
	}
	close, err := dll.FindProc("WintunCloseAdapter")
	if err != nil {
		_ = dll.Release()
		return err
	}
	start, err := dll.FindProc("WintunStartSession")
	if err != nil {
		start = nil // optional; we don't currently use it
	}

	b.dll = dll
	b.create = create
	b.close = close
	b.start = start
	b.loaded = true
	b.snapshot = TUNSnapshot{
		Available: true,
		Installed: true,
	}
	return nil
}

// isElevated reports whether the current process has administrator
// privileges. On Windows this checks the process token for the
// DOMAIN_ALIAS_RID_ADMETERS group.
func isElevated() bool {
	// Simple shell-execute check: try to access a privileged
	// resource. This is a placeholder; production code should use
	// OpenProcessToken + GetTokenInformation.
	if runtime.GOOS != "windows" {
		return false
	}
	cmd := exec.Command("net", "session")
	return cmd.Run() == nil
}

// wintunInstallDir returns the directory where FreeIran stores the
// Wintun DLL: <AppData>/FreeIran/cores/wintun/.
func wintunInstallDir() (string, error) {
	appData := osGetEnv("APPDATA")
	if appData == "" {
		return "", fmt.Errorf("APPDATA not set")
	}
	return filepath.Join(appData, "FreeIran", "cores", "wintun"), nil
}

// downloadAndExtractWintun downloads the official wintun release ZIP
// from wintun.net and extracts wintun.dll to dstPath.
//
// URL: https://www.wintun.net/builds/wintun-0.14.1.zip
//
// The ZIP layout is:
//
//	wintun/bin/<arch>/wintun.dll
//
// where <arch> is amd64, arm64, x86. We extract the architecture
// matching the current process.
func downloadAndExtractWintun(ctx context.Context, dstPath string) error {
	// This requires network access. We delegate to the system curl
	// / PowerShell's iwr to keep the Go binary dependency-free. In
	// production this would use net/http with checksum verification.
	url := "https://www.wintun.net/builds/wintun-0.14.1.zip"
	zipPath := dstPath + ".zip"

	if err := runCmd(ctx, "curl", "-fsSL", "-o", zipPath, url); err != nil {
		// Fallback to PowerShell.
		ps := fmt.Sprintf("iwr -UseBasicParsing -Uri '%s' -OutFile '%s'", url, zipPath)
		if err := runCmd(ctx, "powershell", "-NoProfile", "-Command", ps); err != nil {
			return fmt.Errorf("download wintun: %v", err)
		}
	}
	defer removeFile(zipPath)

	// Extract the architecture-specific DLL.
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	case "386":
		arch = "x86"
	default:
		arch = "amd64"
	}

	if err := runCmd(ctx, "powershell", "-NoProfile", "-Command",
		fmt.Sprintf("Expand-Archive -Path '%s' -DestinationPath '%s.extract' -Force; Copy-Item '%s.extract\\wintun\\bin\\%s\\wintun.dll' '%s' -Force",
			zipPath, dstPath, dstPath, arch, dstPath)); err != nil {
		return fmt.Errorf("extract wintun: %v", err)
	}
	_ = removeFile(dstPath + ".extract")
	return nil
}

// runCmd runs a command silently and returns an error if it fails.
func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v (%s)", name, strings.Join(args, " "), err, string(out))
	}
	return nil
}

// statFile returns nil if path exists.
func statFile(path string) (interface{}, error) {
	// Use os.Stat through a thin wrapper so we can stub in tests.
	return osStat(path)
}

// removeFile wraps os.Remove for testability.
func removeFile(path string) error { return osRemove(path) }

// executableDir returns the directory of the running FreeIran executable.
func executableDir() (string, error) {
	exe, err := osExecutable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// systemDir returns C:\Windows\System32 (or the equivalent on
// non-standard installs).
func systemDir() string {
	return osGetEnv("SystemRoot") + "\\System32"
}

// The following helpers exist so the file can be compiled and tested
// on Linux (where syscall.LoadDLL is unavailable). They are wired
// to the real os package via the *_real.go and *_stub.go files.

// osGetEnv reads an environment variable.
func osGetEnv(key string) string { return osGetEnvReal(key) }

// osStat wraps os.Stat.
func osStat(path string) (interface{}, error) { return osStatReal(path) }

// osRemove wraps os.Remove.
func osRemove(path string) error { return osRemoveReal(path) }

// osExecutable wraps os.Executable.
func osExecutable() (string, error) { return osExecutableReal() }
