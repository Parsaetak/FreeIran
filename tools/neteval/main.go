// Command neteval performs REAL network validation of the v0.9.6
// engine subsystems (§23 of the upgrade specification): discovery
// against the live sources, smart search against the live GitHub API,
// real ping measurements and real URL tests.
//
// It is an OUT-OF-BAND evidence collector: the CI matrix never
// depends on the network, and this tool is never imported by the
// application — `go run ./tools/neteval` is run manually (or by a
// release engineer) against the live Internet. Every number it
// prints is measured; nothing is fabricated.
//
// Usage:
//
//	go run ./tools/neteval            # full run (~2-4 minutes)
//
// The report is printed to stdout and written to
// /tmp/neteval-report.json.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/discovery"
	"github.com/Parsaetak/FreeIran/engine/ranking"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/tester"
	"github.com/Parsaetak/FreeIran/internal/httpx"
)

type report struct {
	StartedAt time.Time `json:"started_at"`

	Discovery struct {
		DurationMS   int64                    `json:"duration_ms"`
		Stats        *discovery.Stats         `json:"stats,omitempty"`
		LevelDetail  []discovery.LevelStats   `json:"levels"`
		SourceHealth []discovery.SourceHealth `json:"source_health"`
	} `json:"discovery"`

	Search struct {
		Attempted  bool     `json:"attempted"`
		DurationMS int64    `json:"duration_ms"`
		Candidates int      `json:"candidates"`
		Repos      []string `json:"repos,omitempty"`
		Error      string   `json:"error,omitempty"`
	} `json:"search"`

	Ping struct {
		Tested    int          `json:"tested"`
		Succeeded int          `json:"succeeded"`
		MedianMS  []int64      `json:"median_ms_samples"`
		Best      *pingSummary `json:"best,omitempty"`
	} `json:"ping"`

	URLTests struct {
		Attempted int          `json:"attempted"`
		OK        int          `json:"ok"`
		Results   []urlSummary `json:"results"`
	} `json:"url_tests"`

	Environment struct {
		Signals []string `json:"signals"`
		Summary string   `json:"summary"`
	} `json:"environment"`

	Limitations []string `json:"limitations"`
}

type pingSummary struct {
	Endpoint string  `json:"endpoint"`
	MedianMS int64   `json:"median_ms"`
	MinMS    int64   `json:"min_ms"`
	MaxMS    int64   `json:"max_ms"`
	JitterMS int64   `json:"jitter_ms"`
	Loss     float64 `json:"loss"`
	Samples  int     `json:"samples"`
}

type urlSummary struct {
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	TotalMS   int64  `json:"total_ms"`
	DNSMS     int64  `json:"dns_ms"`
	ConnectMS int64  `json:"connect_ms"`
	TLSMS     int64  `json:"tls_ms"`
	TTFBMS    int64  `json:"ttfb_ms"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rep := report{StartedAt: time.Now().UTC()}
	rep.Limitations = append(rep.Limitations,
		"Real-tunnel URL tests require an installed protocol core; none is installed in this evaluation environment, so URL tests run DIRECT (proving the measurement machinery) and tunnel-path behaviour is covered by the fake-core end-to-end tests in CI.")

	// ---- 1. REAL discovery against the live sources ----------------
	fmt.Println("== REAL DISCOVERY (configured + trusted public sources) ==")

	eng := discovery.NewEngine(httpx.Default(), discovery.DefaultEngineConfig(), "/tmp/neteval-health.json")

	started := time.Now()

	nodes, stats := eng.Discover(ctx, discovery.FullLevels(), nil, source.DefaultSources())

	rep.Discovery.DurationMS = time.Since(started).Milliseconds()
	rep.Discovery.Stats = stats
	rep.Discovery.LevelDetail = stats.Levels
	rep.Discovery.SourceHealth = eng.Health().Snapshot()

	fmt.Printf("discovery: %d valid nodes, %d duplicates, %d ms\n",
		len(nodes), stats.Duplicates, rep.Discovery.DurationMS)

	for _, l := range stats.Levels {
		fmt.Printf("  level %-14s sources=%d ok=%d fail=%d candidates=%d valid=%d dup=%d (%d ms)\n",
			l.Name, l.Sources, l.FetchedOK, l.Failed, l.Candidates, l.Valid, l.Duplicates, l.DurationMS)
	}

	for _, h := range rep.Discovery.SourceHealth {
		fmt.Printf("  source %-38s avail=%.2f parse=%.2f yield=%.2f dup=%.2f latency=%dms failures=%d\n",
			h.SourceID, h.Availability(), h.ParseSuccess(), h.ValidYield(), h.DuplicateRate(), h.LastLatencyMS, h.ConsecutiveFailures)
	}

	if len(nodes) == 0 {
		fmt.Println("NO nodes discovered — recording and continuing")
	}

	// ---- 2. REAL smart search against the live GitHub API ----------
	fmt.Println("\n== REAL SMART SEARCH (live GitHub API) ==")

	searcher := discovery.NewSearcher(httpx.Default(), discovery.DefaultSearchConfig())

	sStart := time.Now()

	cands, serr := searcher.Search(ctx, discovery.DefaultSearchQueries[:2], time.Now())

	rep.Search.Attempted = true
	rep.Search.DurationMS = time.Since(sStart).Milliseconds()

	if serr != nil {
		rep.Search.Error = serr.Error()
		fmt.Printf("search failed: %v\n", serr)
	} else {
		rep.Search.Candidates = len(cands)

		for _, c := range cands {
			rep.Search.Repos = append(rep.Search.Repos, c.Repo)
		}

		fmt.Printf("search: %d repositories in %d ms\n", len(cands), rep.Search.DurationMS)

		for _, c := range cands {
			fmt.Printf("  %-40s stars=%-5d pushed=%s\n", c.Repo, c.Stars, c.PushedAt.Format("2006-01-02"))
		}
	}

	// ---- 3. REAL ping measurements against live endpoints ----------
	fmt.Println("\n== REAL PING (TCP handshake samples) ==")

	probe := &tester.PingProbe{Samples: 4, Timeout: 3 * time.Second}

	type pr struct {
		endpoint string
		m        config.PingMetrics
	}

	var pings []pr

	limit := len(nodes)
	if limit > 12 {
		limit = 12
	}

	for i := 0; i < limit; i++ {
		n := nodes[i]

		metrics, err := probe.Ping(ctx, n.Address, n.Port)
		if err != nil && ctx.Err() != nil {
			break
		}

		pings = append(pings, pr{endpoint: fmt.Sprintf("%s:%d", n.Address, n.Port), m: metrics})

		rep.Ping.Tested++
		if metrics.Samples > 0 {
			rep.Ping.Succeeded++
			rep.Ping.MedianMS = append(rep.Ping.MedianMS, metrics.MedianMS)
		}
	}

	// Display copy sorted by median; `pings` itself stays in node
	// order so the ranking pairing below stays aligned.
	sortedPings := append([]pr(nil), pings...)

	sort.Slice(sortedPings, func(i, j int) bool {
		return sortedPings[i].m.MedianMS < sortedPings[j].m.MedianMS
	})

	if len(sortedPings) > 0 {
		best := sortedPings[0]

		rep.Ping.Best = &pingSummary{
			Endpoint: best.endpoint,
			MedianMS: best.m.MedianMS,
			MinMS:    best.m.MinMS,
			MaxMS:    best.m.MaxMS,
			JitterMS: best.m.JitterMS,
			Loss:     best.m.PacketLoss,
			Samples:  best.m.Samples,
		}
	}

	for _, p := range sortedPings {
		fmt.Printf("  %-45s median=%-5d min=%-5d max=%-5d jitter=%-4d loss=%.2f samples=%d timeouts=%d\n",
			p.endpoint, p.m.MedianMS, p.m.MinMS, p.m.MaxMS, p.m.JitterMS, p.m.PacketLoss, p.m.Samples, p.m.Timeouts)
	}

	fmt.Printf("ping: %d/%d endpoints answered\n", rep.Ping.Succeeded, rep.Ping.Tested)

	// ---- 4. REAL URL tests (direct — machinery proof) ---------------
	fmt.Println("\n== REAL URL TESTS (direct measurement machinery) ==")

	urler := &tester.URLTester{Timeout: 10 * time.Second}

	targets := []string{
		"https://www.gstatic.com/generate_204",
		"https://cp.cloudflare.com/generate_204",
	}

	for _, t := range targets {
		m := urler.Test(ctx, nil, t)

		rep.URLTests.Attempted++
		if m.OK {
			rep.URLTests.OK++
		}

		rep.URLTests.Results = append(rep.URLTests.Results, urlSummary{
			URL: t, OK: m.OK, Status: m.Status, TotalMS: m.TotalMS,
			DNSMS: m.DNSMS, ConnectMS: m.ConnectMS, TLSMS: m.TLSMS, TTFBMS: m.TTFBMS,
		})

		fmt.Printf("  %-45s ok=%-5v status=%-4d total=%-5dms dns=%-4d connect=%-4d tls=%-4d ttfb=%-4dms\n",
			t, m.OK, m.Status, m.TotalMS, m.DNSMS, m.ConnectMS, m.TLSMS, m.TTFBMS)
	}

	// ---- 5. Ranking over the measured pool --------------------------
	if len(nodes) > 0 {
		fmt.Println("\n== RANKING (measured pool, best overall) ==")

		rich := make([]ranking.RichCandidate, 0, len(nodes))

		for i, n := range nodes {
			if i >= limit {
				break
			}

			rich = append(rich, ranking.RichCandidate{
				Candidate: ranking.Candidate{
					Fingerprint:        n.Fingerprint(),
					Name:               n.Name,
					Protocol:           string(n.Type),
					Endpoint:           fmt.Sprintf("%s:%d", n.Address, n.Port),
					CompatibleBackends: 1,
				},
				Ping: &pings[i].m,
			})
		}

		ranked := ranking.RankMetrics(rich, ranking.SortBestOverall, time.Now().UTC())

		for i, r := range ranked {
			if i >= 5 {
				break
			}

			fmt.Printf("  #%d %-40s overall=%.3f ping=%s(%s) url=%s(%s)\n",
				i+1, r.Candidate.Name, r.Metrics.OverallScore,
				fmt.Sprintf("%.2f", r.Metrics.PingScore), r.Metrics.PingProvenance,
				fmt.Sprintf("%.2f", r.Metrics.URLScore), r.Metrics.URLProvenance)
		}
	}

	raw, _ := json.MarshalIndent(rep, "", "  ")

	_ = os.WriteFile("/tmp/neteval-report.json", raw, 0o600)

	fmt.Println("\nreport written to /tmp/neteval-report.json")
}
