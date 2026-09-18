package tester

import "time"

// latency.go defines FreeIran's canonical latency-measurement
// representation (v0.9.8.1, §2 of the upgrade specification).
//
// PROBLEM IT FIXES (root cause of the Windows CI failure, run
// 35287863799 → engine/tester/TestTCPProbeReachable → "latency
// should be measured"):
//
//   1. Coarse monotonic clocks. On some platforms — most notably
//      Windows CI runners, whose Go runtime monotonic clock
//      (KUSER_SHARED_DATA interrupt time) ticks only once per clock
//      interrupt — an operation that completes within one tick
//      measures time.Since() == 0 even though it demonstrably ran.
//      A successful dial with a raw reading of 0 was stored as
//      Latency == 0, which the old representation could not
//      distinguish from "not measured".
//   2. Truncation ambiguity. Any genuine sub-millisecond measurement
//      (loopback dials, local endpoints) collapsed through
//      time.Duration.Milliseconds() to 0, and 0 was interpreted as
//      "unmeasured" by ranking (obs.LatencyMS > 0), quality
//      classification (QualityFor(0) == "failed") and the UI
//      (latency_ms > 0 ? show : "—"). A 0.5 ms probe — the fastest
//      possible outcome — was classified as FAILED.
//
// REPRESENTATION RULES (the contract every consumer must honour):
//
//   R1. The true time.Duration is preserved internally with full
//       nanosecond precision (Result.Latency, PingMetrics.*).
//   R2. A SUCCESSFUL operation always yields a strictly positive
//       Latency: a raw reading <= 0 is a clock-granularity artifact
//       (the operation completed, so at least one unit of time
//       passed) and is safely quantized up to ClockFloor. No
//       artificial sleeps are ever inserted to inflate readings.
//   R3. Measured (Result.Measured) is the ONLY authoritative "was a
//       latency actually measured" signal. Consumers must NEVER
//       infer measured/unmeasured from a millisecond projection.
//   R4. Millisecond projections (PingMS, DurationMS, LatencyMS,
//       MedianMS, ...) are display/storage conveniences. The value 0
//       with a real measurement means "measured, below one
//       millisecond" — rendered as "< 1 ms" — never "unmeasured".
//   R5. For stored observations without a dedicated flag,
//       Working == true implies a measurement exists
//       (TestObservation.LatencyMS == 0 && Working == true is a
//       sub-millisecond success, not a missing value).
//   R6. Ordering by measured latency sorts sub-millisecond (0 ms
//       projected) candidates FIRST — they are the fastest.

// ClockFloor is the smallest representable measured duration. It is
// the safe quantization applied to successful operations whose raw
// clock reading is zero: the success itself proves at least one
// nanosecond elapsed, so the value remains a truthful lower bound.
const ClockFloor = time.Nanosecond

// MeasuredLatency normalizes the raw elapsed reading of a SUCCESSFUL
// operation into the canonical representation: strictly positive,
// full precision. It must only be called for operations that
// completed successfully; failures carry no measurement.
func MeasuredLatency(raw time.Duration) time.Duration {
	if raw <= 0 {
		return ClockFloor
	}

	return raw
}

// MSOf projects a measured duration onto whole milliseconds for
// display and storage. Sub-millisecond measurements project to 0;
// pair the result with the measured flag (R3/R4) when interpreting.
func MSOf(d time.Duration) int64 {
	return d.Milliseconds()
}

// MSOfQuantized projects a measured duration onto whole milliseconds
// with safe quantization: sub-millisecond measurements report 1,
// never 0. Use ONLY for coarse aggregate statistics whose 0 value is
// a sentinel for "no data yet" and cannot carry a paired measured
// flag (e.g. testqueue's running fastest/slowest/average stats).
func MSOfQuantized(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}

	if ms := d.Milliseconds(); ms > 0 {
		return ms
	}

	return 1
}

// QualityForMeasured classifies a millisecond projection together
// with its measured flag. A measured sub-millisecond latency (0 ms
// projected) is the best band, "excellent" — never "failed".
func QualityForMeasured(ms int64, measured bool) string {
	if !measured || ms < 0 {
		return QualityFailed
	}

	switch {
	case ms <= 150:
		return QualityExcellent
	case ms <= 400:
		return QualityGood
	case ms <= 800:
		return QualityAcceptable
	case ms <= 2000:
		return QualitySlow
	default:
		return QualityVerySlow
	}
}

// QualityForDuration classifies a measured duration directly,
// preserving sub-millisecond precision.
func QualityForDuration(d time.Duration) string {
	if d <= 0 {
		return QualityFailed
	}

	return QualityForMeasured(d.Milliseconds(), true)
}

// LatencyText renders a millisecond projection with its measured
// flag for human display: "82 ms", "< 1 ms" (measured sub-ms) or
// "—" (unmeasured).
func LatencyText(ms int64, measured bool) string {
	if !measured {
		return "—"
	}

	if ms <= 0 {
		return "< 1 ms"
	}

	// ms > 0: render via the caller's formatter is not available here;
	// plain milliseconds keep this dependency-free.
	return itoaDuration(ms) + " ms"
}

func itoaDuration(ms int64) string {
	if ms == 0 {
		return "0"
	}

	neg := ms < 0
	if neg {
		ms = -ms
	}

	var buf [20]byte

	i := len(buf)

	for ms > 0 {
		i--
		buf[i] = byte('0' + ms%10)
		ms /= 10
	}

	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}
