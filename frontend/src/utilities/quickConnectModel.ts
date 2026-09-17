import type { CandidateView } from "../services";

/**
 * v0.9.8 Quick Connect picker model.
 *
 * Pure module: turns the backend's ranked candidate views
 * (ConnectionService.BestCandidates — credential-free, TTL-cached,
 * derived ONLY from recorded test history) into the compact, honestly
 * labelled list the Quick Connect picker renders.
 *
 * Ordering contract (§7 of the v0.9.8 spec):
 *   1. verified usable candidates first;
 *   2. then by MEASURED ping (never source-provided, never estimated —
 *      CandidateView.latency_ms is the median of real observations);
 *   3. tie-breaks fall back to the ranking engine's own aggregates:
 *      recent success → sample count (stability) → freshness → score;
 *   4. untested (class "unknown") candidates come after every verified
 *      one and are labelled "— untested" — a low theoretical latency
 *      can never outrank a verified usable candidate;
 *   5. dead / unconnectable candidates are excluded entirely (Quick
 *      Connect is an action surface; the Configurations and Connection
 *      pages remain the places where broken entries are inspected).
 *
 * Determinism: identical inputs produce an identical list; ties break
 * by fingerprint, mirroring engine/ranking's determinism policy.
 */

/** Provenance label for one candidate's latency figure. */
export type CandidateStatus = "verified" | "stale" | "untested";

/**
 * Freshness window shared with the configuration browser (§18):
 * measurements older than 30 minutes are shown, but labelled stale.
 */
export const FRESH_WINDOW_MS = 30 * 60 * 1000;

/**
 * Classifies a candidate's latency provenance. Only actual recorded
 * observations count: no samples ⇒ "untested", samples older than the
 * freshness window ⇒ "stale", otherwise "verified".
 */
export function candidateStatus(
  view: Pick<CandidateView, "samples" | "tested_at">,
  now: number = Date.now(),
): CandidateStatus {
  if (!view.samples || view.samples <= 0 || !view.tested_at) {
    return "untested";
  }

  return now - view.tested_at > FRESH_WINDOW_MS ? "stale" : "verified";
}

/** True when Quick Connect may offer this candidate at all. */
export function isUsableCandidate(
  view: Pick<CandidateView, "connectable" | "class">,
): boolean {
  return view.connectable && view.class !== "dead";
}

/**
 * Orders candidates for the Quick Connect picker (best first).
 * Returns a new array; the input is never mutated. Dead and
 * unconnectable candidates are dropped.
 */
export function orderForQuickConnect(
  views: CandidateView[],
  now: number = Date.now(),
): CandidateView[] {
  const usable = views.filter(isUsableCandidate);

  const verified = usable.filter((view) => candidateStatus(view, now) !== "untested");
  const untested = usable.filter((view) => candidateStatus(view, now) === "untested");

  const byFingerprint = (a: string, b: string): number =>
    a < b ? -1 : a > b ? 1 : 0;

  verified.sort((a, b) => {
    const aMeasured = a.latency_ms > 0;
    const bMeasured = b.latency_ms > 0;

    // 2. measured ping, fastest first (missing ping after measured).
    if (aMeasured && bMeasured && a.latency_ms !== b.latency_ms) {
      return a.latency_ms - b.latency_ms;
    }

    if (aMeasured !== bMeasured) return aMeasured ? -1 : 1;

    // 3-6. the engine's own aggregates, deterministic tie-breaks last.
    if (a.success_rate !== b.success_rate) return b.success_rate - a.success_rate;
    if (a.samples !== b.samples) return b.samples - a.samples;
    if ((a.tested_at ?? 0) !== (b.tested_at ?? 0)) return (b.tested_at ?? 0) - (a.tested_at ?? 0);
    if (a.score !== b.score) return b.score - a.score;

    return byFingerprint(a.fingerprint, b.fingerprint);
  });

  untested.sort((a, b) => {
    if (a.score !== b.score) return b.score - a.score;

    return byFingerprint(a.fingerprint, b.fingerprint);
  });

  return [...verified, ...untested];
}

/** One renderable picker row: everything shown, nothing sensitive. */
export interface QuickCandidateRow {
  fingerprint: string;
  /** Short display name (already credential-free; truncated for the chip). */
  name: string;
  /** Protocol identifier, lower-cased ("vless", "trojan", …). */
  protocol: string;
  /** Measured ping in ms, or 0 when never measured. */
  latencyMS: number;
  status: CandidateStatus;
  /** Ranking-engine class ("best" | "good" | "unstable" | "unknown"). */
  quality: string;
}

/** Maximum candidates surfaced in the compact picker (§9: 5–8 visible, scroll for more). */
export const QUICK_PICKER_LIMIT = 25;

/** Builds the picker rows from ordered candidates. */
export function quickPickerRows(
  ordered: CandidateView[],
  now: number = Date.now(),
): QuickCandidateRow[] {
  return ordered.slice(0, QUICK_PICKER_LIMIT).map((view) => ({
    fingerprint: view.fingerprint,
    name: view.name || view.endpoint || "Unnamed configuration",
    protocol: (view.protocol || "unknown").toLowerCase(),
    latencyMS: view.latency_ms > 0 ? view.latency_ms : 0,
    status: candidateStatus(view, now),
    quality: view.class,
  }));
}

/** Latency cell text: "82 ms" or an honest "—" for the unmeasured. */
export function pickerLatencyText(latencyMS: number): string {
  return latencyMS > 0 ? `${Math.round(latencyMS)} ms` : "—";
}

/** Status chip text for a row ("verified" | "stale" | "untested"). */
export function pickerStatusText(status: CandidateStatus): string {
  return status;
}

/** Chip color class for a row's status dot. */
export function pickerStatusClass(status: CandidateStatus): string {
  switch (status) {
    case "verified":
      return "success";
    case "stale":
      return "warn";
    default:
      return "neutral";
  }
}

/** Human quality label for the ranking class (mirrors Connection page). */
export function qualityLabel(quality: string): string {
  switch (quality) {
    case "best":
      return "Excellent";
    case "good":
      return "Good";
    case "unstable":
      return "Unstable";
    case "dead":
      return "Down";
    default:
      return "Untested";
  }
}
