// @ts-check
// DiscoveryService bindings.
//
// Hand-written v0.9.6 bindings (stable Call.ByName path; see
// networkservice.js for the rationale). Methods stay in sync with
// engine/app/discoveryservice.go. The returned structures
// (StartFlowEvent/Status/Result, discovery.Stats, netcheck.Environment)
// arrive as plain JSON objects; their shapes live in
// src/types/discovery.ts.
/**
 * DiscoveryService exposes the multi-level node discovery engine,
// environment intelligence, source health and the adaptive start
 * flow (START → DETECT → DISCOVER → TEST → RANK → CONNECT → VERIFY).
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.DiscoveryService.";

/**
 * RunStartFlow executes the full adaptive flow. A non-empty
 * manualFingerprint connects exactly that candidate (manual
 * selection overrides automatic selection).
 * @param {string} [manualFingerprint] - optional manual candidate
 * @returns {$CancellablePromise<any>}
 */
export function RunStartFlow(manualFingerprint) {
    return $Call.ByName($prefix + "RunStartFlow", manualFingerprint ?? "");
}

/**
 * CancelStartFlow cancels a running flow (user action).
 * @returns {$CancellablePromise<boolean>}
 */
export function CancelStartFlow() {
    return $Call.ByName($prefix + "CancelStartFlow");
}

/**
 * StartFlowStatus returns the current flow status (stage, message,
 * environment signals, last result).
 * @returns {$CancellablePromise<any>}
 */
export function StartFlowStatus() {
    return $Call.ByName($prefix + "StartFlowStatus");
}

/**
 * DiscoverNow runs one discovery pass (deep=true escalates to the
 * deep level set for restrictive environments) and persists newly
 * discovered candidates.
 * @param {boolean} deep - whether to run deep discovery
 * @returns {$CancellablePromise<any>}
 */
export function DiscoverNow(deep) {
    return $Call.ByName($prefix + "DiscoverNow", deep);
}

/**
 * SourceHealthList returns the measured health of every known
 * source (availability, parse success, valid yield, duplicate rate,
 * freshness, backoff state).
 * @returns {$CancellablePromise<any>}
 */
export function SourceHealthList() {
    return $Call.ByName($prefix + "SourceHealthList");
}

/**
 * Environment returns the cached environment analysis (refreshing it
 * when older than the cache window).
 * @returns {$CancellablePromise<any>}
 */
export function Environment() {
    return $Call.ByName($prefix + "Environment");
}
