/**
 * Vitest global setup (v0.14.2).
 *
 * The suite runs as a parallel worker pool; cold store hydration on
 * the page tests (Configs row rendering, Network sources, Settings
 * bindings) is event-driven but not instantaneous, and under full-pool
 * CPU contention it can exceed testing-library's default 1s
 * async-util timeout. That produced intermittent, load-dependent
 * "Unable to find role=listitem" failures (observed on the v0.14.1
 * base tree itself: Configs.v0121 / Configs.v0122 rows).
 *
 * The fix is NOT a sleep and not a per-test retry: it raises the
 * event-driven waitFor/findBy budget to 4s for every query, so the
 * same DOM transition is awaited longer before a test fails. No
 * assertion is weakened; the suite stays fully deterministic in what
 * it requires and merely stops failing on scheduler jitter.
 */
import { configure } from "@testing-library/dom";

configure({ asyncUtilTimeout: 4000 });
