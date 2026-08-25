package source

// DefaultSources returns the initial public configuration sources.
//
// Sources are deliberately kept as data rather than embedded into the
// collector so the engine can evolve independently of the source list.
//
// Public configuration sources are untrusted input. They are parsed,
// normalized, deduplicated, and later tested before being promoted to
// the active pool.
func DefaultSources() []Source {
	return []Source{
		{
			ID:      "nirevil-vless",
			Name:    "NiREvil VLESS",
			URL:     "https://raw.githubusercontent.com/NiREvil/vless/main/README.md",
			Enabled: true,
		},
		{
			ID:      "morpheusadam-best",
			Name:    "MorpheusAdam Best",
			URL:     "https://raw.githubusercontent.com/morpheusadam/v2ray-config/main/subs/bundles/best.txt",
			Enabled: true,
		},
		{
			ID:      "morpheusadam-iran",
			Name:    "MorpheusAdam Iran",
			URL:     "https://raw.githubusercontent.com/morpheusadam/v2ray-config/main/subs/bundles/iran.txt",
			Enabled: true,
		},
		{
			ID:      "radikal-verified",
			Name:    "0xRadikal Verified",
			URL:     "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/main/verified/configs.txt",
			Enabled: true,
		},
		{
			ID:      "radikal-vless",
			Name:    "0xRadikal VLESS",
			URL:     "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/vless.txt",
			Enabled: true,
		},
		{
			ID:      "radikal-vmess",
			Name:    "0xRadikal VMess",
			URL:     "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/vmess.txt",
			Enabled: true,
		},
		{
			ID:      "radikal-trojan",
			Name:    "0xRadikal Trojan",
			URL:     "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/trojan.txt",
			Enabled: true,
		},
		{
			ID:      "radikal-shadowsocks",
			Name:    "0xRadikal Shadowsocks",
			URL:     "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/shadowsocks.txt",
			Enabled: true,
		},
		{
			ID:      "radikal-hysteria2",
			Name:    "0xRadikal Hysteria2",
			URL:     "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/hysteria2.txt",
			Enabled: true,
		},
		{
			ID:      "radikal-tuic",
			Name:    "0xRadikal TUIC",
			URL:     "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/tuic.txt",
			Enabled: true,
		},
		{
			ID:      "radikal-wireguard",
			Name:    "0xRadikal WireGuard",
			URL:     "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/wireguard.txt",
			Enabled: true,
		},
	}
}
