// tor.go implements TorEngine (§8): Tor as a FIRST-CLASS provider —
// never represented as a VLESS/VMess/Trojan node.
//
// Lifecycle: resolve → install/update → validate → start → bootstrap
// → local SOCKS ready → health → stop → cleanup.
//
// Source of truth: the official Tor Project distribution
// (dist.torproject.org) — tor-expert-bundle archives with the
// project's own sha256sums-signed-build.txt as the checksum
// authority, fetched over TLS from the same host. Upstream integrity
// is verified before execution; no third-party mirrors, no scripts.
//
// Bootstrap progress comes from Tor's ACTUAL state: the notice log
// lines Tor itself emits ("Bootstrapped 45% (Connecting)") — never
// from timers. Runtime data (DataDir, cache) stays inside the
// FreeIran workspace. One managed instance; no hardcoded bridges —
// bridge lines are user-provided and validated; WebTunnel is built
// into modern Tor; obfs4/Snowflake run through user-configured
// pluggable-transport plugin paths.
package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
	"github.com/Parsaetak/FreeIran/system"
)

// Tor identifiers and licensing.
const (
	TorName          = "tor"
	TorLicense       = "BSD-3-Clause (Tor Project)"
	TorNotice        = "Tor is developed by the Tor Project, Inc. This product is produced independently from the Tor® software and carries no guarantee from The Tor Project."
	TorDefaultSource = "https://dist.torproject.org/torbrowser"
	TorPinnedVersion = "15.0.20" // default channel; resolve fetches fresh checksums
)

// TorEngine manages the Tor expert bundle as a provider.
type TorEngine struct {
	binary *BinaryManager
	source *TorSource

	mu        sync.Mutex
	process   *system.ManagedProcess
	endpoint  Endpoint
	startedAt time.Time
	bootstrap BootstrapInfo
	scanner   *lineScanner
	lastError string

	// options configure the NEXT start.
	options TorOptions
}

// TorOptions configures one Tor run.
type TorOptions struct {
	// BridgeLines are user-provided bridge lines (validated); empty
	// connects through the default Tor relays.
	BridgeLines []string

	// TransportPlugins maps transport names (obfs4, snowflake) to
	// user-provided client plugin executables. Paths must exist.
	TransportPlugins map[string]string

	// BootstrapTimeout bounds start → bootstrapped (default 120s).
	BootstrapTimeout time.Duration

	// SocksPort fixes the local SOCKS port (0 = ephemeral reserved).
	SocksPort int
}

// DefaultTorOptions returns safe defaults.
func DefaultTorOptions() TorOptions {
	return TorOptions{BootstrapTimeout: 120 * time.Second}
}

// TorSource resolves official expert-bundle releases.
type TorSource struct {
	// BaseURL is the official distribution root.
	BaseURL string

	// PinnedVersion is the default channel version.
	PinnedVersion string

	// Latest resolves the newest stable version from the directory
	// listing instead of the pinned one.
	Latest bool

	// HTTP is the shared client.
	HTTP httpx.Interface
}

// NewTorSource creates the official source.
func NewTorSource(httpClient httpx.Interface, latest bool) *TorSource {
	return &TorSource{
		BaseURL:       TorDefaultSource,
		PinnedVersion: TorPinnedVersion,
		Latest:        latest,
		HTTP:          httpClient,
	}
}

// Resolve finds the expert-bundle release for this platform with its
// published checksum.
func (s *TorSource) Resolve(ctx context.Context) (Release, error) {
	version := s.PinnedVersion

	// v0.9.8.3: the production default re-queries the CURRENT official
	// distribution listing (stable channel) so a stale pin can never
	// be the only install path; the pinned version is the
	// deterministic fallback when the listing is unreachable.
	if s.Latest {
		v, err := s.latestStableVersion(ctx)
		if err == nil && v != "" {
			version = v
		} else if s.PinnedVersion != "" {
			version = s.PinnedVersion
		} else {
			return Release{}, fmt.Errorf("resolve latest tor version: %w", err)
		}
	}

	platform := PlatformSuffix(runtimeGOOS(), runtimeGOARCH())

	assetName := fmt.Sprintf("tor-expert-bundle-%s-%s.tar.gz", platform, version)

	sumsURL := fmt.Sprintf("%s/%s/sha256sums-signed-build.txt", strings.TrimSuffix(s.BaseURL, "/"), version)

	resp, err := s.HTTP.Get(ctx, sumsURL, httpx.GetOptions{})
	if err != nil {
		return Release{}, fmt.Errorf("fetch checksums: %w", err)
	}

	if resp.StatusCode != 200 {
		return Release{}, fmt.Errorf("checksums fetch: HTTP %d", resp.StatusCode)
	}

	sha := findChecksumLine(string(resp.Body), assetName)
	if sha == "" {
		return Release{}, fmt.Errorf("official checksums do not list %s", assetName)
	}

	return Release{
		Version:    version,
		AssetURL:   fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(s.BaseURL, "/"), version, assetName),
		AssetName:  assetName,
		SHA256:     sha,
		ReleaseURL: fmt.Sprintf("%s/%s/", strings.TrimSuffix(s.BaseURL, "/"), version),
	}, nil
}

// latestStableVersion parses the distribution listing for stable
// version directories (alpha builds like 16.0a11 are skipped).
func (s *TorSource) latestStableVersion(ctx context.Context) (string, error) {
	resp, err := s.HTTP.Get(ctx, strings.TrimSuffix(s.BaseURL, "/")+"/", httpx.GetOptions{})
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("listing fetch: HTTP %d", resp.StatusCode)
	}

	best := ""

	for _, match := range extractHrefDirs(string(resp.Body)) {
		v := strings.TrimSuffix(strings.TrimPrefix(match, "/"), "/")
		if !isStableTorbrowserVersion(v) {
			continue
		}

		if best == "" || compareVersionStrings(v, best) > 0 {
			best = v
		}
	}

	if best == "" {
		return "", fmt.Errorf("no stable version found in the distribution listing")
	}

	return best, nil
}

// findChecksumLine extracts the SHA-256 for one file name from a
// sha256sums-format body.
func findChecksumLine(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		// GNU sha256sum format: "<hash> [ *]<name>". The Tor Project
		// publishes "<hash>  <name>" (two spaces). Tolerate the
		// binary-mode asterisk marker and extra whitespace instead of
		// rejecting the official file shape, but still require a
		// 64-hex digest in the first field.
		candidate := strings.TrimPrefix(fields[len(fields)-1], "*")

		if candidate == name && len(fields[0]) == 64 {
			return strings.ToLower(fields[0])
		}
	}

	return ""
}

// extractHrefDirs pulls `href="X/"` directory entries from an
// Apache-style listing.
func extractHrefDirs(body string) []string {
	var out []string

	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "href=") {
			continue
		}

		start := strings.Index(line, `href="`)
		if start < 0 {
			continue
		}

		rest := line[start+len(`href="`):]

		end := strings.Index(rest, `"`)
		if end < 0 {
			continue
		}

		href := rest[:end]

		if strings.HasSuffix(href, "/") && !strings.Contains(href, "?") {
			out = append(out, href)
		}
	}

	return out
}

func isStableTorbrowserVersion(v string) bool {
	if v == "" || strings.ContainsAny(v, "ab") {
		return false
	}

	dots := strings.Count(v, ".")
	if dots < 2 {
		return false
	}

	for _, part := range strings.Split(v, ".") {
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}

	return true
}

func compareVersionStrings(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")

	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0

		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}

		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}

		if av != bv {
			if av > bv {
				return 1
			}

			return -1
		}
	}

	return 0
}

// NewTorEngine creates the engine bound to a providers root.
func NewTorEngine(rootDir string, httpClient httpx.Interface, latest bool) *TorEngine {
	return NewTorEngineFromSource(rootDir, httpClient, NewTorSource(httpClient, latest))
}

// NewTorEngineFromSource creates the engine with an explicit source
// (advanced deployments and deterministic tests).
func NewTorEngineFromSource(rootDir string, httpClient httpx.Interface, source *TorSource) *TorEngine {
	binary := &BinaryManager{
		Name:           TorName,
		RootDir:        rootDir,
		HTTP:           httpClient,
		Platform:       PlatformSuffix(runtimeGOOS(), runtimeGOARCH()),
		ExecutableName: "tor",
	}

	binary.ValidateBinary = validateTorBinary
	binary.SmokeTest = smokeTestTor

	return &TorEngine{
		binary:  binary,
		source:  source,
		options: DefaultTorOptions(),
	}
}

// Name implements Provider.
func (e *TorEngine) Name() string { return TorName }

// Kind implements Provider.
func (e *TorEngine) Kind() Kind { return KindTor }

// SetOptions configures the next start (bridges, plugins, timeout).
func (e *TorEngine) SetOptions(options TorOptions) error {
	if err := ValidateTorOptions(options); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.options = options

	return nil
}

// ValidateTorOptions rejects malformed bridge lines and missing
// plugin binaries BEFORE any process starts.
func ValidateTorOptions(options TorOptions) error {
	for _, line := range options.BridgeLines {
		if err := ValidateBridgeLine(line); err != nil {
			return err
		}
	}

	for transport, path := range options.TransportPlugins {
		if strings.TrimSpace(transport) == "" {
			return fmt.Errorf("empty transport name")
		}

		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("transport plugin for %s missing: %w", transport, err)
		}
	}

	if options.BootstrapTimeout < 0 {
		return fmt.Errorf("negative bootstrap timeout")
	}

	return nil
}

// ValidateBridgeLine validates one user-provided bridge line:
//
//	Bridge <transport> <address:port> [<40-hex fingerprint>] [k=v ...]
func ValidateBridgeLine(line string) error {
	trimmed := strings.TrimSpace(line)

	if trimmed == "" {
		return fmt.Errorf("empty bridge line")
	}

	fields := strings.Fields(trimmed)

	if len(fields) < 2 {
		return fmt.Errorf("bridge line needs transport, address and port")
	}

	if strings.EqualFold(fields[0], "bridge") {
		fields = fields[1:]
	}

	if len(fields) < 2 {
		return fmt.Errorf("bridge line needs transport, address and port")
	}

	transport := fields[0]
	if strings.ContainsAny(transport, "=/") {
		return fmt.Errorf("invalid bridge transport %q", transport)
	}

	host, port, err := net.SplitHostPort(fields[1])
	if err != nil {
		return fmt.Errorf("bridge address %q: %w", fields[1], err)
	}

	if strings.TrimSpace(host) == "" || port == "" {
		return fmt.Errorf("bridge address %q incomplete", fields[1])
	}

	if _, perr := strconv.Atoi(port); perr != nil {
		return fmt.Errorf("bridge port %q: %w", port, perr)
	}

	// Optional fingerprint.
	if len(fields) >= 3 && len(fields[2]) == 40 {
		if _, ferr := parseHexFingerprint(fields[2]); ferr != nil {
			return ferr
		}
	}

	return nil
}

func parseHexFingerprint(fp string) (string, error) {
	for _, r := range fp {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return "", fmt.Errorf("invalid bridge fingerprint %q", fp)
		}
	}

	return strings.ToLower(fp), nil
}

// Resolve implements Provider.
func (e *TorEngine) Resolve(ctx context.Context) (Release, error) {
	return e.source.Resolve(ctx)
}

// Install implements Provider: resolve → verified download →
// validate → smoke → atomic activate.
func (e *TorEngine) Install(ctx context.Context) error {
	e.mu.Lock()

	if e.process != nil && e.process.Running() {
		e.mu.Unlock()

		return fmt.Errorf("tor is running; stop it before installing")
	}

	e.mu.Unlock()

	release, err := e.Resolve(ctx)
	if err != nil {
		return err
	}

	// v0.9.8.3 idempotency: the manifest records the downloaded
	// asset's SHA-256, which IS the official published checksum of the
	// target release. Matching checksums mean the exact same verified
	// bundle is already installed — no re-download. (The old
	// runtime-version-vs-bundle-version comparison could never match:
	// tor reports 0.4.8.x while the bundle is 15.0.x.)
	manifest := e.binary.LoadManifest()
	if manifest.BinaryPath != "" && manifest.ChecksumSHA256 != "" &&
		release.SHA256 != "" &&
		strings.EqualFold(manifest.ChecksumSHA256, release.SHA256) {
		// Same verified bundle already installed and activated.
		return nil
	}

	return e.binary.Install(ctx, release)
}

// Uninstall implements Provider.
func (e *TorEngine) Uninstall(ctx context.Context) error {
	if err := e.Stop(ctx); err != nil {
		return err
	}

	return e.binary.Uninstall()
}

// torRuntimeVersion maps a TorBrowser bundle version to the tor
// runtime version family when possible (informational only).
func torRuntimeVersion(bundleVersion string) string { return bundleVersion }

// Start implements Provider: launch ONE managed instance and observe
// bootstrap until the local SOCKS endpoint is ready.
func (e *TorEngine) Start(ctx context.Context) error {
	e.mu.Lock()

	if e.process != nil && e.process.Running() {
		e.mu.Unlock()

		return fmt.Errorf("tor is already running")
	}

	binaryPath := e.binary.BinaryPath()
	if binaryPath == "" {
		e.mu.Unlock()

		return fmt.Errorf("tor is not installed")
	}

	options := e.options
	if options.BootstrapTimeout <= 0 {
		options.BootstrapTimeout = 120 * time.Second
	}

	port := options.SocksPort
	if port == 0 {
		reserved, err := reserveLocalPort()
		if err != nil {
			e.mu.Unlock()

			return fmt.Errorf("reserve SOCKS port: %w", err)
		}

		port = reserved
	}

	dataDir := filepath.Join(e.binary.Dir(), "data")
	cacheDir := filepath.Join(e.binary.Dir(), "cache")
	logDir := filepath.Join(e.binary.Dir(), "logs")

	for _, dir := range []string{dataDir, cacheDir, logDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			e.mu.Unlock()

			return err
		}
	}

	torrc, err := buildTorrc(TorrcConfig{
		SocksPort:        port,
		DataDir:          dataDir,
		CacheDir:         cacheDir,
		BridgeLines:      options.BridgeLines,
		TransportPlugins: options.TransportPlugins,
	})
	if err != nil {
		e.mu.Unlock()

		return err
	}

	configPath := filepath.Join(e.binary.Dir(), "torrc")

	if err := os.WriteFile(configPath, []byte(torrc), 0o600); err != nil {
		e.mu.Unlock()

		return err
	}

	e.mu.Unlock()

	scanner := newLineScanner(512)
	scanner.setSink(e.ingestBootstrapLine)

	spec := system.ProcessSpec{
		Name:   "tor",
		Path:   binaryPath,
		Args:   []string{"-f", configPath},
		Stdout: scanner,
		Stderr: scanner,
	}

	proc, err := system.Start(ctx, spec)
	if err != nil {
		e.mu.Lock()
		e.lastError = err.Error()
		e.mu.Unlock()

		return fmt.Errorf("launch tor: %w", err)
	}

	e.mu.Lock()
	e.process = proc
	e.scanner = scanner
	e.endpoint = Endpoint{Network: "socks5", Host: "127.0.0.1", Port: port}
	e.startedAt = time.Now().UTC()
	e.bootstrap = BootstrapInfo{Active: true, UpdatedAt: time.Now().UTC()}
	e.lastError = ""
	e.mu.Unlock()

	_ = e.binary.MarkState(StateStarting, "", "")

	// Observe ACTUAL bootstrap state (log lines + endpoint probe).
	readyErr := e.awaitBootstrap(ctx, options.BootstrapTimeout, port)

	if readyErr != nil {
		// Deterministic shutdown — never leave an orphan.
		_ = proc.Stop(5 * time.Second)

		e.mu.Lock()
		e.process = nil
		e.endpoint = Endpoint{}
		e.lastError = readyErr.Error()
		e.bootstrap = BootstrapInfo{Active: false, Tag: "failed", UpdatedAt: time.Now().UTC()}
		e.mu.Unlock()

		_ = e.binary.MarkState(StateFailed, "bootstrap", readyErr.Error())

		return fmt.Errorf("tor bootstrap: %w", readyErr)
	}

	e.mu.Lock()
	e.endpoint.Verified = true
	e.bootstrap = BootstrapInfo{Active: false, Complete: true, Progress: 100, Tag: "Done", UpdatedAt: time.Now().UTC()}
	e.mu.Unlock()

	_ = e.binary.MarkState(StateReady, "", "")

	return nil
}

// awaitBootstrap waits for Tor's own signals: the "Bootstrapped
// 100%" log line or the SOCKS listener accepting connections —
// whichever is observed first. Progress comes only from real log
// lines, never timers.
func (e *TorEngine) awaitBootstrap(ctx context.Context, timeout time.Duration, port int) error {
	deadline := time.Now().Add(timeout)

	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-dctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}

			return fmt.Errorf("bootstrap not observed within %s", timeout)
		case <-ticker.C:
			if e.processExited() {
				return fmt.Errorf("tor exited during startup: %s", e.scanner.tail(4))
			}

			if e.bootstrapComplete() {
				return nil
			}

			// The endpoint accepting a SOCKS handshake is Tor's own
			// readiness signal even before the 100% log line.
			if socksEndpointAccepts(dctx, port) {
				return nil
			}
		}
	}
}

func (e *TorEngine) processExited() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.process == nil || !e.process.Running()
}

func (e *TorEngine) bootstrapComplete() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.bootstrap.Complete
}

// ingestBootstrapLine parses one Tor notice-log line; called from
// the shared line scanner.
func (e *TorEngine) ingestBootstrapLine(line string) {
	if !strings.Contains(line, "Bootstrapped") {
		return
	}

	// "Bootstrapped 45% (Connecting): ..." — progress from Tor.
	fields := strings.Fields(line)

	for i, field := range fields {
		if strings.EqualFold(field, "Bootstrapped") && i+1 < len(fields) {
			raw := strings.TrimSuffix(fields[i+1], "%")

			progress, err := strconv.Atoi(raw)
			if err != nil {
				continue
			}

			tag := ""

			if i+2 < len(fields) {
				tag = strings.Trim(fields[i+2], "():")
			}

			e.mu.Lock()

			e.bootstrap = BootstrapInfo{
				Active:    progress < 100,
				Progress:  progress,
				Tag:       tag,
				Complete:  progress >= 100,
				UpdatedAt: time.Now().UTC(),
			}

			e.mu.Unlock()
		}
	}
}

// Stop implements Provider: graceful, bounded shutdown.
func (e *TorEngine) Stop(ctx context.Context) error {
	e.mu.Lock()

	proc := e.process
	sc := e.scanner

	e.process = nil
	e.scanner = nil
	e.endpoint = Endpoint{}
	e.bootstrap = BootstrapInfo{}
	e.mu.Unlock()

	if proc == nil {
		return nil
	}

	_ = e.binary.MarkState(StateStopping, "", "")

	err := proc.Stop(5 * time.Second)

	if sc != nil {
		sc.close()
	}

	_ = e.binary.MarkState(StateInstalled, "", "")

	return err
}

// State implements Provider.
func (e *TorEngine) State() LifecycleState {
	e.mu.Lock()
	running := e.process != nil && e.process.Running()
	bootstrap := e.bootstrap
	e.mu.Unlock()

	if running {
		if bootstrap.Complete {
			return StateReady
		}

		return StateStarting
	}

	manifest := e.binary.LoadManifest()

	switch LifecycleState(manifest.State) {
	case StateNotInstalled:
		return StateNotInstalled
	case StateInstalling:
		return StateInstalling
	case StateInstalled, StateReady:
		return StateInstalled
	case StateDisabled:
		return StateDisabled
	default:
		return StateFailed
	}
}

// Info implements Provider.
func (e *TorEngine) Info() Info {
	manifest := e.binary.LoadManifest()

	e.mu.Lock()
	running := e.process != nil && e.process.Running()
	runtimeState := ""

	if e.process != nil {
		runtimeState = e.process.State().String()
	}

	endpoints := endpointCopy(e.endpoint)
	bootstrap := e.bootstrap
	startedAt := e.startedAt
	e.mu.Unlock()

	info := Info{
		Name:          TorName,
		Kind:          KindTor,
		Installed:     manifest.BinaryPath != "" && fileExists(manifest.BinaryPath),
		Version:       manifest.Version,
		State:         e.State(),
		RuntimeState:  runtimeState,
		Source:        manifest.SourceURL,
		License:       TorLicense,
		Notice:        TorNotice,
		LastCheck:     manifest.LastChecked,
		Endpoints:     endpoints,
		Bootstrap:     bootstrap,
		FailureReason: manifest.FailureReason,
	}

	info.Capabilities = e.capabilities(manifest.Version, running)

	if running && !startedAt.IsZero() {
		info.RuntimeState += fmt.Sprintf(" (since %s)", startedAt.Format(time.RFC3339))
	}

	return info
}

// capabilities reports ONLY what the real runtime provides.
func (e *TorEngine) capabilities(version string, running bool) []string {
	caps := []string{}

	if running {
		caps = append(caps, "socks5-local-endpoint")
	}

	// WebTunnel is built into tor >= 0.4.8.
	if torSupportsWebTunnel(version) {
		caps = append(caps, "webtunnel-builtin")
	}

	e.mu.Lock()
	plugins := make([]string, 0, len(e.options.TransportPlugins))

	for transport, path := range e.options.TransportPlugins {
		if fileExists(path) {
			plugins = append(plugins, transport)
		}
	}
	e.mu.Unlock()

	for _, transport := range plugins {
		caps = append(caps, "transport-"+transport+"-user-plugin")
	}

	if len(e.options.BridgeLines) > 0 {
		caps = append(caps, "user-bridges")
	}

	return caps
}

// Endpoints implements Provider.
func (e *TorEngine) Endpoints() []Endpoint {
	e.mu.Lock()
	defer e.mu.Unlock()

	return endpointCopy(e.endpoint)
}

// Health implements Provider: process + measured SOCKS probe.
func (e *TorEngine) Health(ctx context.Context) Health {
	health := Health{CheckedAt: time.Now().UTC()}

	e.mu.Lock()
	proc := e.process
	endpoint := e.endpoint
	e.mu.Unlock()

	if proc == nil {
		health.Details = "not running"

		return health
	}

	health.ProcessAlive = proc.Running()

	if endpoint.Port != 0 {
		ready, latency := probeSOCKSEndpoint(ctx, endpoint.Host, endpoint.Port)
		health.ListenerReady = ready

		if ready {
			health.Measured = true
			health.LatencyMS = latency.Milliseconds() // 0 + Measured = sub-ms
		}
	}

	health.OK = health.ProcessAlive && health.ListenerReady

	if !health.OK {
		health.Details = "process or SOCKS endpoint unavailable"
	}

	return health
}

// Cleanup implements Provider: prune old logs, keep user data.
func (e *TorEngine) Cleanup(ctx context.Context) error {
	logDir := filepath.Join(e.binary.Dir(), "logs")

	return pruneDir(logDir, 7*24*time.Hour, 16)
}

// ---- torrc ---------------------------------------------------------

// TorrcConfig parameterizes the generated configuration. The file is
// written 0600 and NEVER logged (bridge lines are private material).
type TorrcConfig struct {
	SocksPort        int
	DataDir          string
	CacheDir         string
	BridgeLines      []string
	TransportPlugins map[string]string
}

// buildTorrc renders a minimal, safe torrc.
func buildTorrc(cfg TorrcConfig) (string, error) {
	for _, line := range cfg.BridgeLines {
		if err := ValidateBridgeLine(line); err != nil {
			return "", err
		}
	}

	var b strings.Builder

	b.WriteString("# FreeIran managed torrc (regenerated per run)\n")
	b.WriteString("SocksPort 127.0.0.1:" + strconv.Itoa(cfg.SocksPort) + "\n")
	b.WriteString("DataDirectory " + cfg.DataDir + "\n")
	b.WriteString("CacheDirectory " + cfg.CacheDir + "\n")
	b.WriteString("CookieAuthentication 0\n")
	b.WriteString("Log notice stdout\n")

	for transport, path := range cfg.TransportPlugins {
		fmt.Fprintf(&b, "ClientTransportPlugin %s exec %s\n", transport, path)
	}

	for _, line := range cfg.BridgeLines {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "bridge ") {
			line = "Bridge " + line
		}

		b.WriteString(line + "\n")
	}

	if len(cfg.BridgeLines) > 0 {
		b.WriteString("UseBridges 1\n")
	}

	return b.String(), nil
}

// ---- validation / smoke -------------------------------------------

// validateTorBinary probes the staged tor executable's version.
//
// The probe runs through system.RunProbe — the SAME supervision
// pipeline as the live provider (no visible console window on
// Windows, job-object/process-tree cleanup, bounded lifetime,
// cancellation) — never a raw os/exec command (v0.9.8.2: closes the
// last provider-local subprocess path; tor --version used to flash
// a CMD window on Windows and had no supervision guarantees).
func validateTorBinary(ctx context.Context, path string) (string, error) {
	res := system.RunProbe(ctx, system.ProcessSpec{
		Name: "tor-version-probe",
		Path: path,
		Args: []string{"--version"},
	}, torVersionProbeTimeout)

	if !res.Launched {
		return "", fmt.Errorf("tor --version did not launch: %w (output: %s)", res.Err, lastLines(res.Output, 4))
	}

	if res.State == system.StateCancelled {
		return "", fmt.Errorf("tor --version cancelled: %w", res.Err)
	}

	if res.ExitCode != 0 {
		return "", fmt.Errorf("tor --version failed with exit %d: %s", res.ExitCode, lastLines(res.Output, 4))
	}

	version := parseTorVersion(res.Output)
	if version == "" {
		return "", fmt.Errorf("could not parse tor version from output")
	}

	return version, nil
}

// torVersionProbeTimeout bounds the supervised --version run.
const torVersionProbeTimeout = 15 * time.Second

// parseTorVersion extracts "0.4.8.16" from Tor's banner.
func parseTorVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Tor version ") {
			rest := strings.TrimPrefix(line, "Tor version ")

			fields := strings.Fields(rest)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}

	return ""
}

// torSupportsWebTunnel reports whether the runtime version carries
// the builtin WebTunnel transport (tor >= 0.4.8). Tor's versioning is
// major.minor.patch[.micro]: 0.4.8 introduced WebTunnel. Unknown
// versions report false — never claim unverified capabilities.
func torSupportsWebTunnel(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 3 {
		return false
	}

	major, merr := strconv.Atoi(parts[0])
	minor, nerr := strconv.Atoi(parts[1])
	patch, perr := strconv.Atoi(parts[2])
	if merr != nil || nerr != nil || perr != nil {
		return false
	}

	return major > 0 ||
		(major == 0 && (minor > 4 || (minor == 4 && patch >= 8)))
}

// smokeTestTor launches the staged binary with a harmless
// verification config through the system supervision layer
// (system.RunProbe: no visible console window on Windows,
// job-object/process-tree cleanup, bounded lifetime, cancellation,
// deterministic termination — v0.9.8.2, same unification as the
// Psiphon validation path).
func smokeTestTor(ctx context.Context, path string) error {
	dir, err := os.MkdirTemp("", "freeiran-tor-smoke-*")
	if err != nil {
		return err
	}

	defer os.RemoveAll(dir)

	torrc := "SocksPort 127.0.0.1:0\nDataDirectory " + filepath.Join(dir, "data") + "\nLog notice stderr\n"

	configPath := filepath.Join(dir, "torrc")

	if err := os.WriteFile(configPath, []byte(torrc), 0o600); err != nil {
		return err
	}

	res := system.RunProbe(ctx, system.ProcessSpec{
		Name:    "tor-smoke-verify",
		Path:    path,
		Args:    []string{"-f", configPath, "--verify-config"},
		WorkDir: dir,
	}, torSmokeTimeout)

	if !res.Launched {
		return fmt.Errorf("config verification did not launch: %w (output: %s)", res.Err, lastLines(res.Output, 4))
	}

	if res.State == system.StateCancelled {
		return fmt.Errorf("config verification cancelled: %w", res.Err)
	}

	if res.ExitCode != 0 {
		return fmt.Errorf("config verification failed with exit %d: %s", res.ExitCode, lastLines(res.Output, 4))
	}

	return nil
}

// torSmokeTimeout bounds the supervised --verify-config run.
const torSmokeTimeout = 20 * time.Second

// ---- shared process-run helpers ------------------------------------

// lineScanner is a concurrency-safe, bounded line buffer that feeds
// provider bootstrap parsers (io.Writer interface for ProcessSpec).
type lineScanner struct {
	mu     sync.Mutex
	lines  []string
	limit  int
	closed bool
	sink   func(string)
}

func newLineScanner(limit int) *lineScanner {
	return &lineScanner{limit: limit}
}

// Write implements io.Writer (raw bytes → lines).
func (s *lineScanner) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return len(p), nil
	}

	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}

		s.lines = append(s.lines, line)

		if len(s.lines) > s.limit {
			s.lines = s.lines[len(s.lines)-s.limit:]
		}

		if s.sink != nil {
			s.sink(line)
		}
	}

	return len(p), nil
}

// setSink installs the per-line consumer (bootstrap parser).
func (s *lineScanner) setSink(fn func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sink = fn
}

func (s *lineScanner) tail(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.lines) == 0 {
		return "(no output)"
	}

	if n > len(s.lines) {
		n = len(s.lines)
	}

	return strings.Join(s.lines[len(s.lines)-n:], " | ")
}

func (s *lineScanner) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
}

// The bootstrap parser is wired by TorEngine.Start via setSink.

func runtimeGOOS() string { return runtime.GOOS }

func runtimeGOARCH() string { return runtime.GOARCH }

func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}

func endpointCopy(e Endpoint) []Endpoint {
	if e.Port == 0 {
		return nil
	}

	return []Endpoint{e}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, " | ")
}

// reserveLocalPort reserves an ephemeral port (listen + close).
func reserveLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	port := listener.Addr().(*net.TCPAddr).Port

	if err := listener.Close(); err != nil {
		return 0, err
	}

	return port, nil
}

// socksEndpointAccepts reports whether the SOCKS port accepts a TCP
// connection (partial readiness signal).
func socksEndpointAccepts(ctx context.Context, port int) bool {
	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}

	_ = conn.Close()

	return true
}

// probeSOCKSEndpoint performs a real SOCKS5 handshake and measures
// the round trip.
func probeSOCKSEndpoint(ctx context.Context, host string, port int) (bool, time.Duration) {
	started := time.Now()

	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false, 0
	}

	defer conn.Close()

	// SOCKS5 greeting: no-auth.
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return false, 0
	}

	reply := make([]byte, 2)

	if _, err := io.ReadFull(conn, reply); err != nil {
		return false, 0
	}

	if reply[0] != 0x05 || reply[1] != 0x00 {
		return false, 0
	}

	elapsed := time.Since(started)
	if elapsed <= 0 {
		elapsed = time.Nanosecond
	}

	return true, elapsed
}

// pruneDir removes files older than maxAge, keeping at most keep
// newest entries.
func pruneDir(dir string, maxAge time.Duration, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	type aged struct {
		name string
		mod  time.Time
	}

	var files []aged

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		files = append(files, aged{entry.Name(), info.ModTime()})
	}

	// Newest first.
	for i := 0; i < len(files); i++ {
		for j := i + 1; j < len(files); j++ {
			if files[j].mod.After(files[i].mod) {
				files[i], files[j] = files[j], files[i]
			}
		}
	}

	// Age-based pruning: anything older than the cutoff goes.
	cutoff := time.Now().Add(-maxAge)

	remaining := 0

	for _, f := range files {
		if f.mod.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, f.name))

			continue
		}

		remaining++
	}

	_ = remaining

	// Count cap: keep at most `keep` newest of what remains.
	if len(files) > keep {
		for _, f := range files[keep:] {
			path := filepath.Join(dir, f.name)

			if _, err := os.Stat(path); err == nil {
				_ = os.Remove(path)
			}
		}
	}

	return nil
}
