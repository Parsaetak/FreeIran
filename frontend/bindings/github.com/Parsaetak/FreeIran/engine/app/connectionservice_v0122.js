// @ts-check

/**
 * v0.12.2 — ConnectionService.ConnectChain added AFTER the last
 * machine-generated run (the wails3 generator cannot run on the
 * current host). Hand-maintained ByName binding following the
 * documented repo pattern; the Go bindings contract test verifies the
 * method against the real service.
 *
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.ConnectionService.";

/**
 * ConnectChain establishes a proxy-chain session through the EXISTING
 * verified connection state machine (ONE core process; Xray
 * sockopt.dialerProxy or sing-box detour — never one process per hop).
 *
 * @param {string} chainID
 * @returns {$CancellablePromise<import("../connection/models.js").Snapshot>}
 */
export function ConnectChain(chainID) {
    return $Call.ByName($prefix + "ConnectChain", chainID);
}
