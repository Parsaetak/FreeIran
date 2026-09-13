// @ts-check
// NetworkService bindings.
//
// Hand-written v0.9.0 bindings (stable Call.ByName path; see
// coreservice.js for the rationale). Methods stay in sync with
// engine/app/networkservice.go.
/**
 * NetworkService exposes the Internet / Network Diagnostics
 * capability: manual connection checks, cached reports and the
 * sanitized diagnostic summary.
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.NetworkService.";

/**
 * CheckConnection runs the full probe set (DNS, TCP, HTTPS, latency
 * and — when a session is connected — the proxy path) and returns the
 * classified report.
 * @returns {$CancellablePromise<any>}
 */
export function CheckConnection() {
    return $Call.ByName($prefix + "CheckConnection");
}

/**
 * LastReport returns the most recent report without re-running
 * probes (null when no check has run yet).
 * @returns {$CancellablePromise<any | null>}
 */
export function LastReport() {
    return $Call.ByName($prefix + "LastReport");
}
