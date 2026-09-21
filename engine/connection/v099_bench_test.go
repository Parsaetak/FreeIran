// v099_bench_test.go — v0.9.9 focused benchmarks for the hot paths
// this upgrade touched. Benchmarks measure real behavior on the real
// types; they exist so future changes have a baseline (and so the
// CI "Benchmarks (smoke)" step exercises the code).
package connection

import (
	"testing"
	"time"
)

func benchSnapshot() Snapshot {
	return Snapshot{
		State:         StateConnectedVerified,
		Core:          "xray",
		CoreVersion:   "26.3.27",
		ConfigID:      "5f8ac9d3e2b14c7da09b1c2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a",
		ConfigName:    "bench-node",
		ConfigDisplay: "vless://bench-node@example.org:443",
		Endpoint:      "127.0.0.1:10808",
		LatencyMS:     142,
		StartedAt:     time.Now().UnixMilli(),
		CorePID:       4242,
		CoreReadyMS:   96,
		PingMedianMS:  142,
		URLTotalMS:    380,
		Verification:  "usable",
		VerifiedAt:    time.Now().UnixMilli(),
		Attempts: []Attempt{
			{Backend: "xray", OK: true, Duration: 96, At: time.Now().UTC()},
		},
	}
}

// BenchmarkSnapshotsEqualEqual measures the dedup predicate on two
// identical snapshots (the publisher's hot path: every stateChanged
// pays this once).
func BenchmarkSnapshotsEqualEqual(b *testing.B) {
	a := benchSnapshot()

	bb := a

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if !snapshotsEqual(a, bb) {
			b.Fatal("want equal")
		}
	}
}

// BenchmarkSnapshotsEqualDifferent measures the predicate on
// different snapshots (unequal path: first differing field exits).
func BenchmarkSnapshotsEqualDifferent(b *testing.B) {
	a := benchSnapshot()

	bb := a
	bb.State = StateConnected

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if snapshotsEqual(a, bb) {
			b.Fatal("want unequal")
		}
	}
}
