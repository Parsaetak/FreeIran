// @ts-check

/**
 * v0.12.2 — AppService Quick Connect route-mode capabilities added
 * AFTER the last machine-generated run (the wails3 generator cannot
 * run on the current host). Hand-maintained ByName binding following
 * the documented repo pattern; the Go bindings contract test
 * (engine/app/bindings_contract_test.go) verifies every ByName target
 * against the real service methods.
 *
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.AppService.";

/**
 * ConnectMode returns the persisted Quick Connect route choice
 * (auto | configs | chains). Replaces the removed provider mode.
 *
 * @returns {$CancellablePromise<string>}
 */
export function ConnectMode() {
    return $Call.ByName($prefix + "ConnectMode");
}

/**
 * SetConnectMode persists the Quick Connect route choice through the
 * one settings path and returns the effective mode.
 *
 * @param {string} mode
 * @returns {$CancellablePromise<string>}
 */
export function SetConnectMode(mode) {
    return $Call.ByName($prefix + "SetConnectMode", mode);
}
