package coremgr

// DefaultSources returns the official upstream release sources for
// every managed core. Each core is an independent project with its
// own release cadence and archive layout; the manager treats them as
// distinct, never as interchangeable.
//
// Source selection policy:
//
//   - GitHub Releases API only (no third-party mirrors, no raw CDN
//     guesses). The API returns verified asset URLs and metadata.
//   - Asset selection uses patterns ordered by preference. The first
//     pattern that matches an existing asset name wins.
//   - The same ReleaseAPI URL serves both stable and prerelease
//     channels: stable uses /releases/latest, prerelease iterates
//     /releases and includes prereleases.
//
// Reference layouts (verified as of 2026-09-13):
//
//	Xray-core:
//	  repo:   XTLS/Xray-core
//	  asset:  Xray-windows-64.zip   (Windows amd64)
//	          Xray-linux-64.zip    (Linux amd64)
//	          Xray-macos-64.zip    (macOS amd64)
//	          Xray-macos-arm64.zip (macOS arm64)
//	  check:  xray run -test -c <file>
//	  run:    xray run -c <file>
//	  probe:  xray version
//
//	V2Fly v2ray-core (v5+):
//	  repo:   v2fly/v2ray-core
//	  asset:  v2ray-windows-64.zip (Windows amd64)
//	          v2ray-linux-64.zip   (Linux amd64)
//	          v2ray-macos-64.zip   (macOS amd64)
//	          v2ray-macos-arm64.zip
//	  check:  v2ray test -c <file>
//	  run:    v2ray run -c <file>
//	  probe:  v2ray version
//
//	SagerNet sing-box:
//	  repo:   SagerNet/sing-box
//	  asset:  sing-box-1.x.x-windows-amd64.zip
//	          sing-box-1.x.x-linux-amd64.tar.gz
//	          sing-box-1.x.x-darwin-amd64.tar.gz
//	          sing-box-1.x.x-darwin-arm64.tar.gz
//	  check:  sing-box check -c <file>
//	  run:    sing-box run -c <file>
//	  probe:  sing-box version
func DefaultSources() map[CoreName]Source {
	return map[CoreName]Source{
		CoreXray: {
			Name:             CoreXray,
			DisplayName:      "Xray-core",
			Repo:             "XTLS/Xray-core",
			ReleaseAPI:       "https://api.github.com/repos/XTLS/Xray-core/releases",
			ReleasePage:      "https://github.com/XTLS/Xray-core/releases",
			AssetPatterns:    []string{"windows-64.zip", "linux-64.zip", "macos-64.zip", "macos-arm64.zip"},
			VersionProbeArgs: []string{"version"},
			ConfigCheckArgs:  []string{"run", "-test", "-c"},
			RunArgs:          []string{"run", "-c"},
			MinVersion:       "1.8.0",
		},
		CoreV2Ray: {
			Name:             CoreV2Ray,
			DisplayName:      "V2Ray-core (V2Fly)",
			Repo:             "v2fly/v2ray-core",
			ReleaseAPI:       "https://api.github.com/repos/v2fly/v2ray-core/releases",
			ReleasePage:      "https://github.com/v2fly/v2ray-core/releases",
			AssetPatterns:    []string{"windows-64.zip", "linux-64.zip", "macos-64.zip", "macos-arm64.zip"},
			VersionProbeArgs: []string{"version"},
			ConfigCheckArgs:  []string{"test", "-c"},
			RunArgs:          []string{"run", "-c"},
			MinVersion:       "5.0.0",
		},
		CoreSingBox: {
			Name:        CoreSingBox,
			DisplayName: "sing-box",
			Repo:        "SagerNet/sing-box",
			ReleaseAPI:  "https://api.github.com/repos/SagerNet/sing-box/releases",
			ReleasePage: "https://github.com/SagerNet/sing-box/releases",
			AssetPatterns: []string{
				"windows-amd64.zip", "windows-amd64-v3.zip",
				"linux-amd64.tar.gz", "linux-amd64-v3.tar.gz",
				"darwin-amd64.tar.gz", "darwin-arm64.tar.gz",
			},
			VersionProbeArgs: []string{"version"},
			ConfigCheckArgs:  []string{"check", "-c"},
			RunArgs:          []string{"run", "-c"},
			MinVersion:       "1.10.0",
		},
	}
}

// AssetPatternForPlatform returns the asset name fragment expected to
// match a release asset for the given platform. This is a heuristic:
// the manager iterates every pattern from the Source and picks the
// first asset whose name contains the pattern AND matches the current
// OS/arch.
//
// The pattern picker is platform-aware so the manager does not need
// to know the exact release file names ahead of time.
func AssetPatternForPlatform(p Platform) string {
	switch p.OS {
	case "windows":
		switch p.Arch {
		case "amd64", "x86_64":
			return "windows-amd64"
		case "arm64":
			return "windows-arm64"
		default:
			return "windows-64"
		}
	case "linux":
		switch p.Arch {
		case "amd64", "x86_64":
			return "linux-amd64"
		case "arm64":
			return "linux-arm64"
		default:
			return "linux-64"
		}
	case "darwin":
		switch p.Arch {
		case "amd64", "x86_64":
			return "darwin-amd64"
		case "arm64":
			return "darwin-arm64"
		default:
			return "macos-64"
		}
	default:
		return p.OS + "-" + p.Arch
	}
}
