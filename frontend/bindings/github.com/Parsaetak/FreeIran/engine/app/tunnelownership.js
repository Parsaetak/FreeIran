// @ts-check
// Hand-maintained ByName binding for the v0.10.2 TunnelService
// ownership-status surface (Windows system-proxy UX §15). The
// generated tunnelservice.js cannot be extended without the wails3
// generator; this module carries the ONE added call under the same
// service prefix and the same contract-test verification.

/**
 * Ownership/Recovery status of the system-proxy marker: whether
 * FreeIran owns the system proxy, the ownership phase, the recorded
 * endpoint and the saved previous state.
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.TunnelService.";

/**
 * OwnershipStatus exposes the durable system-proxy ownership marker
 * without touching the platform proxy settings.
 * @returns {$CancellablePromise<{present: boolean, phase?: string, schema_version?: number, endpoint?: string, enabled_at_ms?: number, previous?: {enabled?: boolean, server?: string, bypass?: string[], autoconfig_url?: string, autodetect?: boolean}}>}
 */
export function OwnershipStatus() {
    return $Call.ByName($prefix + "OwnershipStatus");
}
