package core

// This file records the protocol-core versions FreeIran's adapters
// were developed and verified against. The pins are documentation +
// diagnostics metadata: runtime discovery always queries the locally
// installed binary for its actual version, and the adapters contain
// no version-gated behaviour beyond the capability declarations.
//
// Deliberate version policy (docs/development.md):
//   - no "latest" resolution anywhere;
//   - releases verified against these pins are recorded here;
//   - compatibility windows are declared per backend so operators can
//     see which core series the adapter targets.

// PinnedCore describes one verified protocol-core release.
type PinnedCore struct {
	// Name is the backend identifier ("xray", "v2ray", "sing-box").
	Name string

	// Version is the release the adapter was verified against.
	Version string

	// Source is the official distribution repository.
	Source string

	// MinVersion is the lowest series the adapter is expected to
	// support (advisory, surfaced in diagnostics).
	MinVersion string
}

// PinnedCores are the verified reference releases for v0.4.0.
//
// Verification performed against these builds (see
// engine/core/*/config_test.go and docs/development.md):
//   - V2Ray 5.53.0: V4-format config accepted (vless/vmess/trojan/
//     shadowsocks/socks/http outbounds; tcp/ws/grpc/http/quic
//     transports; tls with uTLS fingerprint), `v2ray test`,
//     `v2ray version`, `v2ray run -c`, REALITY absent.
//   - Xray 26.3.27: V4-format config accepted plus REALITY and
//     xtls-rprx-vision flow; plain QUIC/HTTP transports REMOVED in
//     favour of XHTTP; `xray run -test -c`, `xray --version`.
//   - sing-box 1.14.0: native config accepted for all supported
//     protocols incl. REALITY/vision; `sing-box check -c`,
//     `sing-box version`, mixed inbound.
var PinnedCores = []PinnedCore{
	{
		Name:       "xray",
		Version:    "26.3.27",
		Source:     "https://github.com/XTLS/Xray-core",
		MinVersion: "1.8.0",
	},
	{
		Name:       "v2ray",
		Version:    "5.53.0",
		Source:     "https://github.com/v2fly/v2ray-core",
		MinVersion: "5.0.0",
	},
	{
		Name:       "sing-box",
		Version:    "1.14.0",
		Source:     "https://github.com/SagerNet/sing-box",
		MinVersion: "1.10.0",
	},
}

// PinnedVersion returns the verified reference version of a backend
// ("" when unknown).
func PinnedVersion(name string) string {
	for _, pinned := range PinnedCores {
		if pinned.Name == name {
			return pinned.Version
		}
	}

	return ""
}
