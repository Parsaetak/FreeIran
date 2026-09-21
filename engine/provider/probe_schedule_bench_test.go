package provider

import "testing"

// BenchmarkProbeScheduleStep measures the adaptive schedule's step
// cost (one timer reset — the per-loop allocation the pre-0.9.9 fixed
// tickers did not have but a naive time.After loop would pay).
func BenchmarkProbeScheduleStep(b *testing.B) {
	s := newProbeSchedule()
	defer s.stop()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s.next()
	}
}
