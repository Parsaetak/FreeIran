// @ts-check
// CoreService bindings.
//
// Hand-written v0.8 bindings (the wails3 generator requires a GUI
// toolchain on the generating host). They use the runtime's stable
// Call.ByName path — 'package.Service.Method' — which the generated
// bindings resolve to as well; only the numeric-ID fast path differs.
// Regenerate with `wails3 generate bindings` when convenient; the
// method names must stay in sync with engine/app/v6_services.go.
/**
 * CoreService manages the lifecycle of managed protocol-core
 * installations: discovery, install, update, rollback and repair.
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.CoreService.";

/**
 * List returns the manifests of every managed core.
 * @returns {$CancellablePromise<any[]>}
 */
export function List() {
    return $Call.ByName($prefix + "List");
}

/**
 * Info returns the manifest of one managed core.
 * @param {string} name
 * @returns {$CancellablePromise<any>}
 */
export function Info(name) {
    return $Call.ByName($prefix + "Info", name);
}

/**
 * Install downloads and stages a managed core.
 * @param {string} name
 * @returns {$CancellablePromise<void>}
 */
export function Install(name) {
    return $Call.ByName($prefix + "Install", name);
}

/**
 * Uninstall removes a managed core.
 * @param {string} name
 * @returns {$CancellablePromise<void>}
 */
export function Uninstall(name) {
    return $Call.ByName($prefix + "Uninstall", name);
}

/**
 * HealthCheck probes one managed core.
 * @param {string} name
 * @returns {$CancellablePromise<any>}
 */
export function HealthCheck(name) {
    return $Call.ByName($prefix + "HealthCheck", name);
}

/**
 * HealthCheckAll probes every managed core.
 * @returns {$CancellablePromise<Record<string, any>>}
 */
export function HealthCheckAll() {
    return $Call.ByName($prefix + "HealthCheckAll");
}

/**
 * CheckForUpdates looks for a newer release of one core.
 * @param {string} name
 * @returns {$CancellablePromise<any>}
 */
export function CheckForUpdates(name) {
    return $Call.ByName($prefix + "CheckForUpdates", name);
}

/**
 * CheckAllForUpdates looks for newer releases of every core.
 * @returns {$CancellablePromise<any[]>}
 */
export function CheckAllForUpdates() {
    return $Call.ByName($prefix + "CheckAllForUpdates");
}

/**
 * Rollback reverts a core to its previous installed version.
 * @param {string} name
 * @returns {$CancellablePromise<void>}
 */
export function Rollback(name) {
    return $Call.ByName($prefix + "Rollback", name);
}

/**
 * Repair re-validates and re-stages a damaged core installation.
 * @param {string} name
 * @returns {$CancellablePromise<void>}
 */
export function Repair(name) {
    return $Call.ByName($prefix + "Repair", name);
}

/**
 * SetChannel switches a core between stable and beta release channels.
 * @param {string} name
 * @param {string} channel
 * @returns {$CancellablePromise<void>}
 */
export function SetChannel(name, channel) {
    return $Call.ByName($prefix + "SetChannel", name, channel);
}

/**
 * Disable excludes a core from backend selection.
 * @param {string} name
 * @returns {$CancellablePromise<void>}
 */
export function Disable(name) {
    return $Call.ByName($prefix + "Disable", name);
}

/**
 * Enable re-includes a core in backend selection.
 * @param {string} name
 * @returns {$CancellablePromise<void>}
 */
export function Enable(name) {
    return $Call.ByName($prefix + "Enable", name);
}
