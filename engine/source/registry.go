package source

// DefaultSources returns the initial public configuration sources.
//
// Sources are deliberately kept as data rather than embedded into the
// collector so the engine can evolve independently of the source list.
//
// Public configuration sources are untrusted input. They are parsed,
// normalized, deduplicated, and later tested before being promoted to
// the active pool.
//
// v0.6 source registry changes:
//
//  1. Every source carries full metadata (provider, project, protocol
//     hints, region, format, priority, refresh interval) so the UI
//     can show a real Source Manager page instead of just an ID list.
//  2. Three additional sources were added per the v0.6 spec:
//     - ShadowsocksAggregator/Eternity (mixed protocols, global)
//     - MahsaFreeConfig/mtn (Iran-focused, mtn ISP sub)
//     - ScrapeAndCategorize/Netherlands (NL-region, mixed)
//  3. URLs are the raw.githubusercontent.com endpoints ONLY. The HTML
//     /blob/ and /blame/ pages are never used as data sources — they
//     return the GitHub wrapper HTML, not the raw file content.
//  4. Priority defaults: Iran-relevant sources get priority 50, mixed
//     global sources get priority 100, low-priority large dumps get
//     priority 200.
func DefaultSources() []Source {
	return []Source{
		{
			ID:              "nirevil-vless",
			Name:            "NiREvil VLESS",
			URL:             "https://raw.githubusercontent.com/NiREvil/vless/main/README.md",
			Enabled:         true,
			Provider:        "NiREvil",
			Project:         "NiREvil/vless",
			ProtocolHints:   []string{"vless"},
			Region:          "global",
			Format:          "auto",
			Priority:        100,
			RefreshInterval: 0, // global cadence
		},
		{
			ID:            "morpheusadam-best",
			Name:          "MorpheusAdam Best",
			URL:           "https://raw.githubusercontent.com/morpheusadam/v2ray-config/main/subs/bundles/best.txt",
			Enabled:       true,
			Provider:      "MorpheusAdam",
			Project:       "morpheusadam/v2ray-config",
			ProtocolHints: []string{"vless", "vmess", "trojan", "ss"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      100,
		},
		{
			ID:            "morpheusadam-iran",
			Name:          "MorpheusAdam Iran",
			URL:           "https://raw.githubusercontent.com/morpheusadam/v2ray-config/main/subs/bundles/iran.txt",
			Enabled:       true,
			Provider:      "MorpheusAdam",
			Project:       "morpheusadam/v2ray-config",
			ProtocolHints: []string{"vless", "vmess", "trojan", "ss"},
			Region:        "iran",
			Format:        "v2ray-subscription",
			Priority:      50, // Iran-relevant: higher priority
		},
		{
			ID:            "radikal-verified",
			Name:          "0xRadikal Verified",
			URL:           "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/main/verified/configs.txt",
			Enabled:       true,
			Provider:      "0xRadikal",
			Project:       "0xRadikal/Free-v2ray-Configs",
			ProtocolHints: []string{"vless", "vmess", "trojan", "ss"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      100,
		},
		{
			ID:            "radikal-vless",
			Name:          "0xRadikal VLESS",
			URL:           "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/vless.txt",
			Enabled:       true,
			Provider:      "0xRadikal",
			Project:       "0xRadikal/Free-v2ray-Configs",
			ProtocolHints: []string{"vless"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      100,
		},
		{
			ID:            "radikal-vmess",
			Name:          "0xRadikal VMess",
			URL:           "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/vmess.txt",
			Enabled:       true,
			Provider:      "0xRadikal",
			Project:       "0xRadikal/Free-v2ray-Configs",
			ProtocolHints: []string{"vmess"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      100,
		},
		{
			ID:            "radikal-trojan",
			Name:          "0xRadikal Trojan",
			URL:           "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/trojan.txt",
			Enabled:       true,
			Provider:      "0xRadikal",
			Project:       "0xRadikal/Free-v2ray-Configs",
			ProtocolHints: []string{"trojan"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      100,
		},
		{
			ID:            "radikal-shadowsocks",
			Name:          "0xRadikal Shadowsocks",
			URL:           "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/shadowsocks.txt",
			Enabled:       true,
			Provider:      "0xRadikal",
			Project:       "0xRadikal/Free-v2ray-Configs",
			ProtocolHints: []string{"ss"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      100,
		},
		{
			ID:            "radikal-hysteria2",
			Name:          "0xRadikal Hysteria2",
			URL:           "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/hysteria2.txt",
			Enabled:       true,
			Provider:      "0xRadikal",
			Project:       "0xRadikal/Free-v2ray-Configs",
			ProtocolHints: []string{"hysteria2"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      100,
		},
		{
			ID:            "radikal-tuic",
			Name:          "0xRadikal TUIC",
			URL:           "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/tuic.txt",
			Enabled:       true,
			Provider:      "0xRadikal",
			Project:       "0xRadikal/Free-v2ray-Configs",
			ProtocolHints: []string{"tuic"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      100,
		},
		{
			ID:            "radikal-wireguard",
			Name:          "0xRadikal WireGuard",
			URL:           "https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/data/protocols/wireguard.txt",
			Enabled:       true,
			Provider:      "0xRadikal",
			Project:       "0xRadikal/Free-v2ray-Configs",
			ProtocolHints: []string{"wireguard"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      100,
		},

		// --- v0.6 added public sources ---

		{
			ID:            "shadowsocks-aggregator-eternity",
			Name:          "ShadowsocksAggregator Eternity",
			URL:           "https://raw.githubusercontent.com/mahdibland/ShadowsocksAggregator/master/Eternity.txt",
			Enabled:       true,
			Provider:      "mahdibland",
			Project:       "mahdibland/ShadowsocksAggregator",
			ProtocolHints: []string{"vless", "vmess", "trojan", "ss", "hysteria2", "tuic"},
			Region:        "global",
			Format:        "v2ray-subscription",
			Priority:      50, // large, curated, frequently updated
		},
		{
			ID:            "mahsa-free-config-mtn",
			Name:          "MahsaFreeConfig MTN",
			URL:           "https://raw.githubusercontent.com/mahsanet/MahsaFreeConfig/main/mtn/sub_1.txt",
			Enabled:       true,
			Provider:      "mahsanet",
			Project:       "mahsanet/MahsaFreeConfig",
			ProtocolHints: []string{"vless", "vmess", "trojan"},
			Region:        "iran",
			Format:        "v2ray-subscription",
			Priority:      50, // Iran-focused: high priority
		},
		{
			ID:            "scrape-and-categorize-netherlands",
			Name:          "ScrapeAndCategorize Netherlands",
			URL:           "https://raw.githubusercontent.com/10ium/ScrapeAndCategorize/refs/heads/main/output_configs/Netherlands.txt",
			Enabled:       true,
			Provider:      "10ium",
			Project:       "10ium/ScrapeAndCategorize",
			ProtocolHints: []string{"vless", "vmess", "trojan", "ss"},
			Region:        "netherlands",
			Format:        "v2ray-subscription",
			Priority:      100,
		},
	}
}
