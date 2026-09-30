// @ts-check

/**
 * v0.12.1 — targeted SourceService capabilities added AFTER the last
 * machine-generated run (the wails3 generator cannot run on the
 * current host). Hand-maintained ByName binding following the
 * documented repo pattern; the Go bindings contract test
 * (engine/app/bindings_contract_test.go) verifies every ByName target
 * against the real service methods.
 *
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.SourceService.";

/**
 * RefreshSource refreshes EXACTLY ONE source through the SAME
 * ingestion architecture (one fetcher, one parser pipeline, one
 * store, one scheduler — single-owner gate shared with full cycles).
 * Respects the enabled policy and returns the source's bounded stats.
 *
 * @param {string} id
 * @returns {$CancellablePromise<import("../source/models.js").Stats | null>}
 */
export function RefreshSource(id) {
    return $Call.ByName($prefix + "RefreshSource", id);
}
