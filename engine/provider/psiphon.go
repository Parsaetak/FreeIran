// psiphon.go implements PsiphonEngine (§9): Psiphon as a FIRST-CLASS
// provider — never an ordinary node protocol.
//
// Lifecycle: resolve → install/update → validate → start → tunnel
// negotiation → local proxy ready → health → stop → cleanup.
//
// The engine drives the OFFICIAL Psiphon client/tunnel-core
// distribution (the open-source console client from Psiphon's
// published binary channels); the protocol is never reimplemented.
// Two acquisition paths, both integrity-enforced:
//
//   - Managed download from a checksum-publishing official release
//     channel (GitHub releases with asset digests). When the channel
//     publishes no release assets with digests, Resolve reports the
//     honest "unavailable" error and Install refuses — binaries are
//     never executed unverified.
//   - A user-provided binary path, validated by a real smoke launch
//     before activation.
//
// Licensing/attribution (Psiware license for tunnel-core): recorded
// in Info.Notice and surfaced in the UI; nothing is statically
// embedded into FreeIran's binary.
//
// Readiness is observed from the real runtime: the local SOCKS/HTTP
// proxy ports actually accepting connections (and the client's own
// tunnel-up output when it emits it). Reported capabilities are only
// those discovered from the installed runtime.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
	"github.com/Parsaetak/FreeIran/system"
)

// Psiphon identifiers and licensing.
const (
	PsiphonName             = "psiphon"
	PsiphonLicense          = "Psiware License (Psiphon tunnel-core)"
	PsiphonNotice           = "Psiphon tunnel-core is developed by Psiphon Inc. This application distributes and runs it as an independent process under its license; no code is statically embedded."
	PsiphonDefaultRepo      = "Psiphon-Labs/psiphon-tunnel-core"
	PsiphonDefaultAssetGlob = "consoleclient"
)

// PsiphonEngine manages the Psiphon console client as a provider.
type PsiphonEngine struct {
	binary *BinaryManager
	source *GitHubReleaseSource

	mu        sync.Mutex
	process   *system.ManagedProcess
	endpoints []Endpoint
	scanner   *lineScanner
	startedAt time.Time
	bootstrap BootstrapInfo
	options   PsiphonOptions
	lastError string
}

// PsiphonOptions configures one run.
type PsiphonOptions struct {
	// NegotiateTimeout bounds start → proxies ready (default 90s).
	NegotiateTimeout time.Duration

	// SocksPort / HTTPPort fix the local proxy ports (0 = ephemeral).
	SocksPort int
	HTTPPort  int

	// ExtraConfig is appended to the generated client config JSON
	// (advanced, user-provided; validated as JSON).
	ExtraConfig string
}

// DefaultPsiphonOptions returns safe defaults.
func DefaultPsiphonOptions() PsiphonOptions {
	return PsiphonOptions{NegotiateTimeout: 90 * time.Second}
}

// GitHubReleaseSource resolves a release channel that publishes
// per-asset digests (the same integrity model engine/coremgr uses).
type GitHubReleaseSource struct {
	// Repo is "owner/name".
	Repo string

	// APIBase is the GitHub API root.
	APIBase string

	// AssetPattern is a lowercase substring the asset name must
	// contain (platform-suffixed by the caller).
	AssetPattern string

	// HTTP is the shared client.
	HTTP httpx.Interface
}

// NewGitHubReleaseSource creates a release source.
func NewGitHubReleaseSource(httpClient httpx.Interface, repo, assetPattern string) *GitHubReleaseSource {
	return &GitHubReleaseSource{
		Repo:         repo,
		APIBase:      "https://api.github.com",
		AssetPattern: strings.ToLower(assetPattern),
		HTTP:         httpClient,
	}
}

// githubReleaseDoc is the subset of the releases API document used.
type githubReleaseDoc struct {
	TagName     string           `json:"tag_name"`
	HTMLURL     string           `json:"html_url"`
	PublishedAt string           `json:"published_at"`
	Prerelease  bool             `json:"prerelease"`
	Assets      []githubAssetDoc `json:"assets"`
}

type githubAssetDoc struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
}

// Resolve finds the newest release asset carrying a SHA-256 digest.
func (s *GitHubReleaseSource) Resolve(ctx context.Context) (Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=10", strings.TrimSuffix(s.APIBase, "/"), s.Repo)

	resp, err := s.HTTP.Get(ctx, url, httpx.GetOptions{})
	if err != nil {
		return Release{}, fmt.Errorf("resolve %s: %w", s.Repo, err)
	}

	if resp.StatusCode != 200 {
		return Release{}, fmt.Errorf("resolve %s: HTTP %d (channel unreachable or rate-limited)", s.Repo, resp.StatusCode)
	}

	var releases []githubReleaseDoc

	if err := json.Unmarshal(resp.Body, &releases); err != nil {
		return Release{}, fmt.Errorf("parse releases: %w", err)
	}

	platform := PlatformSuffix(runtimeGOOS(), runtimeGOARCH())

	for _, release := range releases {
		for _, asset := range release.Assets {
			name := strings.ToLower(asset.Name)

			if !strings.Contains(name, s.AssetPattern) {
				continue
			}

			if !strings.Contains(name, platform) &&
				!strings.Contains(name, "windows-x86_64") && runtime.GOOS == "windows" {
				continue
			}

			digest := normalizeDigest(asset.Digest)
			if digest == "" {
				continue // no checksum authority → not installable
			}

			return Release{
				Version:     strings.TrimPrefix(release.TagName, "v"),
				AssetURL:    asset.BrowserDownloadURL,
				AssetName:   asset.Name,
				Size:        asset.Size,
				SHA256:      digest,
				ReleaseURL:  release.HTMLURL,
				PublishedAt: parseGitHubTime(release.PublishedAt),
			}, nil
		}
	}

	return Release{}, fmt.Errorf(
		"no release asset with a published SHA-256 digest found in %s for platform %s; "+
			"provide the binary manually via settings (validated before use) — "+
			"unverified downloads are never executed", s.Repo, platform)
}

func normalizeDigest(digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return ""
	}

	if idx := strings.Index(digest, ":"); idx >= 0 {
		digest = digest[idx+1:]
	}

	digest = strings.ToLower(strings.TrimSpace(digest))

	if len(digest) != 64 {
		return ""
	}

	for _, r := range digest {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return ""
		}
	}

	return digest
}

func parseGitHubTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}

	return t.UTC()
}

// NewPsiphonEngine creates the engine bound to a providers root.
func NewPsiphonEngine(rootDir string, httpClient httpx.Interface) *PsiphonEngine {
	return NewPsiphonEngineFromSource(rootDir, httpClient,
		NewGitHubReleaseSource(httpClient, PsiphonDefaultRepo, PsiphonDefaultAssetGlob))
}

// NewPsiphonEngineFromSource creates the engine with an explicit
// release source (advanced deployments and deterministic tests).
func NewPsiphonEngineFromSource(rootDir string, httpClient httpx.Interface, source *GitHubReleaseSource) *PsiphonEngine {
	binary := &BinaryManager{
		Name:           PsiphonName,
		RootDir:        rootDir,
		HTTP:           httpClient,
		Platform:       PlatformSuffix(runtimeGOOS(), runtimeGOARCH()),
		ExecutableName: "psiphon-tunnel-core-" + PlatformSuffix(runtimeGOOS(), runtime.GOARCH),
	}

	binary.ValidateBinary = validatePsiphonBinary
	binary.SmokeTest = smokeTestPsiphon

	if source == nil {
		source = NewGitHubReleaseSource(httpClient, PsiphonDefaultRepo, PsiphonDefaultAssetGlob)
	}

	return &PsiphonEngine{
		binary:  binary,
		source:  source,
		options: DefaultPsiphonOptions(),
	}
}

// Name implements Provider.
func (e *PsiphonEngine) Name() string { return PsiphonName }

// Kind implements Provider.
func (e *PsiphonEngine) Kind() Kind { return KindPsiphon }

// SetOptions configures the next start.
func (e *PsiphonEngine) SetOptions(options PsiphonOptions) error {
	if options.NegotiateTimeout < 0 {
		return fmt.Errorf("negative negotiate timeout")
	}

	if options.ExtraConfig != "" {
		var probe map[string]any
		if err := json.Unmarshal([]byte(options.ExtraConfig), &probe); err != nil {
			return fmt.Errorf("extra config is not valid JSON: %w", err)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.options = options

	return nil
}

// Resolve implements Provider.
func (e *PsiphonEngine) Resolve(ctx context.Context) (Release, error) {
	return e.source.Resolve(ctx)
}

// Install implements Provider (managed, digest-verified).
func (e *PsiphonEngine) Install(ctx context.Context) error {
	e.mu.Lock()

	if e.process != nil && e.process.Running() {
		e.mu.Unlock()

		return fmt.Errorf("psiphon is running; stop it before installing")
	}

	e.mu.Unlock()

	release, err := e.Resolve(ctx)
	if err != nil {
		return err
	}

	manifest := e.binary.LoadManifest()
	if manifest.BinaryPath != "" && manifest.Version == release.Version {
		return nil
	}

	return e.binary.Install(ctx, release)
}

// SetUserBinary adopts a user-provided binary path: validated with a
// real smoke launch before it becomes the active provider binary.
func (e *PsiphonEngine) SetUserBinary(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("binary not found: %w", err)
	}

	version, err := validatePsiphonBinary(ctx, path)
	if err != nil {
		return fmt.Errorf("binary did not pass validation: %w", err)
	}

	if err := smokeTestPsiphon(ctx, path); err != nil {
		return fmt.Errorf("binary did not pass the smoke launch: %w", err)
	}

	target := filepath.Join(e.binary.BinDir(), filepath.Base(path))

	if err := moveFile(path, target); err != nil {
		return err
	}

	sum, err := fileSHA256(target)
	if err != nil {
		return err
	}

	manifest := e.binary.LoadManifest()
	manifest.Name = PsiphonName
	manifest.BinaryPath = target
	manifest.Version = version
	manifest.ChecksumSHA256 = sum
	manifest.SourceURL = "user-provided"
	manifest.InstalledAt = time.Now().UTC()
	manifest.State = string(StateInstalled)
	manifest.FailureReason = ""
	manifest.FailureStage = ""

	return e.binary.saveManifest(manifest)
}

// Uninstall implements Provider.
func (e *PsiphonEngine) Uninstall(ctx context.Context) error {
	if err := e.Stop(ctx); err != nil {
		return err
	}

	return e.binary.Uninstall()
}

// Start implements Provider: launch ONE managed console client and
// observe the local proxy endpoints until ready.
func (e *PsiphonEngine) Start(ctx context.Context) error {
	e.mu.Lock()

	if e.process != nil && e.process.Running() {
		e.mu.Unlock()

		return fmt.Errorf("psiphon is already running")
	}

	binaryPath := e.binary.BinaryPath()
	if binaryPath == "" {
		e.mu.Unlock()

		return fmt.Errorf("psiphon is not installed (no verified binary; run Install or provide one via settings)")
	}

	options := e.options
	if options.NegotiateTimeout <= 0 {
		options.NegotiateTimeout = 90 * time.Second
	}

	socksPort, httpPort := options.SocksPort, options.HTTPPort

	var err error

	if socksPort == 0 {
		if socksPort, err = reserveLocalPort(); err != nil {
			e.mu.Unlock()

			return fmt.Errorf("reserve SOCKS port: %w", err)
		}
	}

	if httpPort == 0 {
		if httpPort, err = reserveLocalPort(); err != nil {
			e.mu.Unlock()

			return fmt.Errorf("reserve HTTP port: %w", err)
		}
	}

	dataDir := filepath.Join(e.binary.Dir(), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		e.mu.Unlock()

		return err
	}

	configPath, err := writePsiphonConfig(e.binary.Dir(), socksPort, httpPort, dataDir, options.ExtraConfig)
	if err != nil {
		e.mu.Unlock()

		return err
	}

	e.mu.Unlock()

	scanner := newLineScanner(512)
	scanner.setSink(e.ingestTunnelLine)

	spec := system.ProcessSpec{
		Name:   "psiphon",
		Path:   binaryPath,
		Args:   []string{"-config", configPath},
		Stdout: scanner,
		Stderr: scanner,
	}

	proc, err := system.Start(ctx, spec)
	if err != nil {
		e.mu.Lock()
		e.lastError = err.Error()
		e.mu.Unlock()

		return fmt.Errorf("launch psiphon: %w", err)
	}

	e.mu.Lock()
	e.process = proc
	e.scanner = scanner
	e.endpoints = []Endpoint{
		{Network: "socks5", Host: "127.0.0.1", Port: socksPort},
		{Network: "http", Host: "127.0.0.1", Port: httpPort},
	}
	e.startedAt = time.Now().UTC()
	e.bootstrap = BootstrapInfo{Active: true, UpdatedAt: time.Now().UTC()}
	e.lastError = ""
	e.mu.Unlock()

	_ = e.binary.MarkState(StateStarting, "", "")

	readyErr := e.awaitReady(ctx, options.NegotiateTimeout, socksPort, httpPort)

	if readyErr != nil {
		_ = proc.Stop(5 * time.Second) // never leave orphans

		e.mu.Lock()
		e.process = nil
		e.endpoints = nil
		e.lastError = readyErr.Error()
		e.bootstrap = BootstrapInfo{Tag: "failed", UpdatedAt: time.Now().UTC()}
		e.mu.Unlock()

		_ = e.binary.MarkState(StateFailed, "negotiate", readyErr.Error())

		return fmt.Errorf("psiphon tunnel negotiation: %w", readyErr)
	}

	e.mu.Lock()
	for i := range e.endpoints {
		e.endpoints[i].Verified = true
	}

	e.bootstrap = BootstrapInfo{Complete: true, Progress: 100, Tag: "tunnels up", UpdatedAt: time.Now().UTC()}
	e.mu.Unlock()

	_ = e.binary.MarkState(StateReady, "", "")

	return nil
}

// awaitReady observes the REAL runtime: both local proxy ports must
// accept connections within the negotiate window (the client's own
// "tunnels" output, when emitted, is reflected in bootstrap tags).
func (e *PsiphonEngine) awaitReady(ctx context.Context, timeout time.Duration, socksPort, httpPort int) error {
	deadline := time.Now().Add(timeout)

	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-dctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}

			return fmt.Errorf("local proxies not ready within %s (client output: %s)",
				timeout, e.scannerTail())
		case <-ticker.C:
			if e.psiphonExited() {
				return fmt.Errorf("psiphon exited during startup: %s", e.scannerTail())
			}

			if endpointAccepts(dctx, socksPort) && endpointAccepts(dctx, httpPort) {
				return nil
			}
		}
	}
}

func (e *PsiphonEngine) psiphonExited() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.process == nil || !e.process.Running()
}

func (e *PsiphonEngine) scannerTail() string {
	e.mu.Lock()
	sc := e.scanner
	e.mu.Unlock()

	if sc == nil {
		return "(no output)"
	}

	return sc.tail(4)
}

// ingestTunnelLine reflects the client's own tunnel-up output when
// it emits any (e.g. lines mentioning "tunnels" or "up").
func (e *PsiphonEngine) ingestTunnelLine(line string) {
	lower := strings.ToLower(line)

	if strings.Contains(lower, "tunnels") || strings.Contains(lower, "\"up\"") {
		e.mu.Lock()

		e.bootstrap = BootstrapInfo{
			Active:    !e.bootstrap.Complete,
			Progress:  100,
			Tag:       "tunnels reported by client",
			Complete:  e.bootstrap.Complete,
			UpdatedAt: time.Now().UTC(),
		}

		e.mu.Unlock()
	}
}

// Stop implements Provider.
func (e *PsiphonEngine) Stop(ctx context.Context) error {
	e.mu.Lock()

	proc := e.process
	sc := e.scanner

	e.process = nil
	e.scanner = nil
	e.endpoints = nil
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
func (e *PsiphonEngine) State() LifecycleState {
	e.mu.Lock()
	running := e.process != nil && e.process.Running()
	complete := e.bootstrap.Complete
	e.mu.Unlock()

	if running {
		if complete {
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
func (e *PsiphonEngine) Info() Info {
	manifest := e.binary.LoadManifest()

	e.mu.Lock()
	running := e.process != nil && e.process.Running()
	runtimeState := ""

	if e.process != nil {
		runtimeState = e.process.State().String()
	}

	endpoints := append([]Endpoint(nil), e.endpoints...)
	bootstrap := e.bootstrap
	e.mu.Unlock()

	info := Info{
		Name:          PsiphonName,
		Kind:          KindPsiphon,
		Installed:     manifest.BinaryPath != "" && fileExists(manifest.BinaryPath),
		Version:       manifest.Version,
		State:         e.State(),
		RuntimeState:  runtimeState,
		Source:        manifest.SourceURL,
		License:       PsiphonLicense,
		Notice:        PsiphonNotice,
		LastCheck:     manifest.LastChecked,
		Endpoints:     endpoints,
		Bootstrap:     bootstrap,
		FailureReason: manifest.FailureReason,
	}

	if running {
		info.Capabilities = []string{"socks5-local-endpoint", "http-local-endpoint"}
	} else {
		// Capabilities only from the real runtime — none when not
		// running (never claim protocol support the installed client
		// has not demonstrated).
		info.Capabilities = nil
	}

	return info
}

// Endpoints implements Provider.
func (e *PsiphonEngine) Endpoints() []Endpoint {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]Endpoint(nil), e.endpoints...)
}

// Health implements Provider.
func (e *PsiphonEngine) Health(ctx context.Context) Health {
	health := Health{CheckedAt: time.Now().UTC()}

	e.mu.Lock()
	proc := e.process
	endpoints := append([]Endpoint(nil), e.endpoints...)
	e.mu.Unlock()

	if proc == nil {
		health.Details = "not running"

		return health
	}

	health.ProcessAlive = proc.Running()

	latencyTotal := time.Duration(0)
	measured := 0

	for _, endpoint := range endpoints {
		if endpoint.Network != "socks5" {
			continue
		}

		ready, latency := probeSOCKSEndpoint(ctx, endpoint.Host, endpoint.Port)
		if ready {
			health.ListenerReady = true
			health.Measured = true
			latencyTotal += latency
			measured++
		}
	}

	if measured > 0 {
		health.LatencyMS = (latencyTotal / time.Duration(measured)).Milliseconds()
	}

	health.OK = health.ProcessAlive && health.ListenerReady

	if !health.OK {
		health.Details = "process or proxy endpoints unavailable"
	}

	return health
}

// Cleanup implements Provider.
func (e *PsiphonEngine) Cleanup(ctx context.Context) error {
	logDir := filepath.Join(e.binary.Dir(), "logs")

	return pruneDir(logDir, 7*24*time.Hour, 16)
}

// ---- config / validation ------------------------------------------

// writePsiphonConfig renders the console-client config JSON.
func writePsiphonConfig(dir string, socksPort, httpPort int, dataDir, extra string) (string, error) {
	base := map[string]any{
		"LocalSocksProxyPort":    socksPort,
		"LocalHttpProxyPort":     httpPort,
		"DataRootDirectory":      dataDir,
		"DisableLocalSocksProxy": false,
		"DisableLocalHttpProxy":  false,
	}

	if strings.TrimSpace(extra) != "" {
		var extraMap map[string]any
		if err := json.Unmarshal([]byte(extra), &extraMap); err != nil {
			return "", fmt.Errorf("extra config is not a JSON object: %w", err)
		}

		for key, value := range extraMap {
			// The engine-owned fields are not overridable (port/data
			// binding keeps the provider inside its workspace).
			switch key {
			case "LocalSocksProxyPort", "LocalHttpProxyPort", "DataRootDirectory":
				continue
			}

			base[key] = value
		}
	}

	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, "psiphon.config.json")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}

	return path, nil
}

// validatePsiphonBinary probes the staged binary. The console client
// exposes version via `-version` when supported; a missing flag is
// not fatal (the smoke launch is the real gate) but the version is
// then honestly empty.
func validatePsiphonBinary(ctx context.Context, path string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, path, "-version")

	output, _ := cmd.Output() //nolint:errcheck // version flag support varies

	version := parsePsiphonVersion(string(output))

	return version, nil
}

// parsePsiphonVersion extracts a "tunnel-core x.y.z" style version.
func parsePsiphonVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)

		if idx := strings.Index(lower, "tunnel-core"); idx >= 0 {
			fields := strings.Fields(line)

			for _, field := range fields {
				if looksLikeVersion(field) {
					return strings.TrimPrefix(field, "v")
				}
			}
		}
	}

	return ""
}

func looksLikeVersion(s string) bool {
	if s == "" || !strings.Contains(s, ".") {
		return false
	}

	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}

		for _, r := range part {
			if (r < '0' || r > '9') && r != '-' && r != 'v' {
				return false
			}
		}
	}

	return true
}

// smokeTestPsiphon launches the binary with a throwaway config and
// verifies it starts and terminates deterministically.
func smokeTestPsiphon(ctx context.Context, path string) error {
	dir, err := os.MkdirTemp("", "freeiran-psiphon-smoke-*")
	if err != nil {
		return err
	}

	defer os.RemoveAll(dir)

	socksPort, err := reserveLocalPort()
	if err != nil {
		return err
	}

	httpPort, err := reserveLocalPort()
	if err != nil {
		return err
	}

	configPath, err := writePsiphonConfig(dir, socksPort, httpPort, filepath.Join(dir, "data"), "")
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Launch and confirm the process stays alive briefly, then
	// terminate it — the smoke test never leaves orphans.
	cmd := exec.CommandContext(runCtx, path, "-config", configPath)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("smoke launch failed: %w", err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil && runCtx.Err() == nil {
			// Immediate crash: unusable binary.
			return fmt.Errorf("smoke launch exited immediately: %w", err)
		}
	case <-time.After(1500 * time.Millisecond):
		// Alive after 1.5s: healthy enough for staging. Terminate.
		if runtime.GOOS == "windows" {
			_ = cmd.Process.Kill()
		} else {
			_ = cmd.Process.Kill()
		}
	}

	<-done // join — no orphan processes

	return nil
}

func endpointAccepts(ctx context.Context, port int) bool {
	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}

	_ = conn.Close()

	return true
}
