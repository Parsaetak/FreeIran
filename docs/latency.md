# Latency Measurement Semantics (v0.9.8.1)

This document is the canonical reference for how FreeIran represents,
stores, projects, orders and displays latency measurements. The
contract is implemented in `engine/tester/latency.go` (§2 of the
upgrade specification) and enforced by `engine/tester/latency_test.go`
and `engine/ranking/subms_test.go`. Every consumer listed below was
migrated to the contract in v0.9.8.1.

## The defect this fixes

The Windows CI job (run 35287863799,
`engine/tester/TestTCPProbeReachable`, failure
`"latency should be measured"`) exposed a representation defect with
two compounding causes:

1. **Coarse monotonic clocks.** On some platforms — most notably
   Windows CI runners — the Go runtime's monotonic clock ticks only
   once per clock interrupt, so an operation that completes within one
   tick measures `time.Since() == 0` even though it demonstrably ran.
   A successful dial with a raw reading of 0 was stored as
   `Latency == 0`, which the old representation could not distinguish
   from "not measured".
2. **Truncation ambiguity.** Any genuine sub-millisecond measurement
   (loopback dials, local endpoints) collapsed through
   `time.Duration.Milliseconds()` to 0, and 0 was interpreted as
   "unmeasured" everywhere: quality classification (`QualityFor(0) ==
   "failed"`), ranking (dropping observations with
   `Working && LatencyMS > 0`), and the UI (`latency_ms > 0` gating).
   A 0.5 ms probe — the fastest possible outcome — was classified as
   failed and hidden.

The fix changes the REPRESENTATION, not the test expectations: the
Windows test now passes because a successful probe always yields a
positive measured latency, not because the assertion was relaxed.

## Representation rules (R1–R6)

| Rule | Contract |
|------|----------|
| R1 | The true `time.Duration` is preserved internally with full nanosecond precision (`Result.Latency`, `PingMetrics.*`). |
| R2 | A SUCCESSFUL operation always yields a strictly positive latency: a raw reading ≤ 0 is a clock-granularity artifact (the operation completed, so at least one unit of time passed) and is safely quantized up to `ClockFloor`. No artificial sleeps are ever inserted to inflate readings. |
| R3 | `Result.Measured` is the ONLY authoritative "was a latency actually measured" signal. Consumers must NEVER infer measured/unmeasured from a millisecond projection. |
| R4 | Millisecond projections (`PingMS`, `DurationMS`, `LatencyMS`, `MedianMS`, …) are display/storage conveniences. The value 0 paired with `Measured=true` means "measured, below one millisecond" — rendered `< 1 ms` — never "unmeasured". |
| R5 | For stored observations without a dedicated flag, `Working == true` implies a measurement exists: `TestObservation.LatencyMS == 0 && Working == true` is a sub-millisecond success, not a missing value. |
| R6 | Ordering by measured latency sorts sub-millisecond (0 ms projected) candidates FIRST — they are the fastest. |

`ClockFloor` is `time.Nanosecond`: the smallest representable measured
duration and the safe quantization for successful operations whose
raw clock reading is zero. The success itself proves at least one
nanosecond elapsed, so the value remains a truthful lower bound.

## Projection helpers (`engine/tester/latency.go`)

- `MeasuredLatency(raw)` — normalizes the raw elapsed reading of a
  SUCCESSFUL operation into the canonical representation (R2). Must
  only be called for operations that completed successfully; failures
  carry no measurement.
- `MSOf(d)` — projects a measured duration onto whole milliseconds
  (sub-ms projects to 0; pair with the measured flag per R3/R4).
- `MSOfQuantized(d)` — projects with safe quantization: sub-ms reports
  1, never 0. Use ONLY where a 0 value is a "no data yet" sentinel
  that cannot carry a paired measured flag (testqueue's running
  fastest/slowest/average statistics).
- `QualityForMeasured(ms, measured)` — classifies a projection
  together with its flag; a measured sub-millisecond latency is the
  best band, never "failed".
- `LatencyText(ms, measured)` — the display contract renderer.

## Consumer contract

| Field | Semantics | Consumers |
|-------|-----------|-----------|
| `Result.Latency` | true `time.Duration`, strictly positive on success (R1/R2) | `engine/tester` probes |
| `Result.Measured` | authoritative measured flag (R3) | all result consumers |
| `latency_ms` + `latency_ms_measured` | 0 + true = "measured, sub-ms" (R4); rendered `< 1 ms` | Quick Connect picker (`CandidateView`), frontend ordering/display |
| `TestObservation.LatencyMS` | 0 with `Working == true` is a sub-ms success (R5) | `engine/ranking` (sub-ms working observations included), `engine/ranking/scores.go` (`Score.LatencyMSMeasured`) |
| `PingMetrics.SubMS` | true sub-millisecond sample count | `engine/tester/ping.go` |
| `ModeResult` spawn-to-ready duration | true duration, quantized per R2 | `engine/tester/modes.go` |
| testqueue running stats | 1 ms-quantized (sentinel-safe, `MSOfQuantized`) | `engine/testqueue` |
| `core.Instance.Health` latency | ≥ 1 ms when the listener is ready | `engine/core` |
| netcheck probe RTT | measured flag preserved | `engine/netcheck` probes |
| provider health latency | measured flag preserved | `engine/provider` health |
| configuration sorting | measured-first partition; sub-ms first within measured (R6) | `engine/ranking` (`cmpPing`), `engine/app` (`sortConfigs`), frontend picker model |

## Display contract

A measured sub-millisecond latency renders as `< 1 ms` — never as
`0 ms`, never as a dash, never as "unmeasured". A dash (or `—`) is
reserved for genuinely unmeasured values (`Measured == false`), which
sort below every measured value. The renderer is
`LatencyText(ms, measured)` in `engine/tester/latency.go`; the
frontend Quick Connect model applies the same contract to
`CandidateView.latency_ms` + `latency_ms_measured`
(`frontend/src/utilities/quickConnectModel.ts`).

## Ordering rules

1. Measured candidates partition BEFORE unmeasured ones (the
   measured-first partition in `engine/ranking`'s ping sorts,
   `cmpPing`, applied by `engine/app`'s `sortConfigs`).
2. Within the measured partition, sub-millisecond (0 ms projected)
   candidates sort FIRST (R6) — they are the fastest outcomes.
3. Unmeasured candidates sort last and display a dash.

## Test matrix

`engine/tester/latency_test.go` pins the representation across:
0 / 500 µs / 999 µs / 1 ms / 1.5 ms / 10 ms / 600 ms / 3 s raw
durations, timeout and failure (no measurement — `Measured == false`),
cancellation, and a live loopback dial asserting the representation
(the Windows CI case: a successful dial always yields positive
measured latency regardless of clock granularity).
`engine/ranking/subms_test.go` pins the consumer side: sub-ms working
observations are included in ranking, `LatencyMSMeasured` is reported,
and the measured-first ordering holds. The frontend sub-ms ordering
contract is covered by `frontend/src/utilities/quickConnectModel-subms.test.ts`.
