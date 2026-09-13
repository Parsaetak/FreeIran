// @ts-check
// TunnelService bindings.
//
// Hand-written v0.8 bindings (see coreservice.js for the rationale:
// stable Call.ByName path until the wails3 generator is available on
// a GUI toolchain host). Method names stay in sync with
// engine/app/v6_services.go.
/**
 * TunnelService controls the system-level integration modes: the
 * system proxy and the TUN device.
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.TunnelService.";

/**
 * State returns the current tunnel mode (off / system_proxy / tun).
 * @returns {$CancellablePromise<any>}
 */
export function State() {
    return $Call.ByName($prefix + "State");
}

/**
 * EnableSystemProxy routes system traffic through the local proxy.
 * @param {string} host
 * @param {number} port
 * @param {boolean} asHTTP
 * @param {string[] | null} bypass
 * @returns {$CancellablePromise<void>}
 */
export function EnableSystemProxy(host, port, asHTTP, bypass) {
    return $Call.ByName($prefix + "EnableSystemProxy", host, port, asHTTP, bypass);
}

/**
 * EnableTUN routes system traffic through the TUN device.
 * @param {string} host
 * @param {number} port
 * @returns {$CancellablePromise<void>}
 */
export function EnableTUN(host, port) {
    return $Call.ByName($prefix + "EnableTUN", host, port);
}

/**
 * Disable restores the direct (untunnelled) system state.
 * @returns {$CancellablePromise<void>}
 */
export function Disable() {
    return $Call.ByName($prefix + "Disable");
}
