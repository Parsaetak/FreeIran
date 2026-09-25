// @ts-check
// Hand-maintained ByName binding for the v0.10.2 personal
// configuration import service. Same pattern as profileservice.js:
// the wails3 generator cannot run on the current host, so the module
// follows the documented contract format the bindings contract test
// enforces statically (a $prefix constant plus one $Call.ByName
// invocation per exported Go method).

/**
 * ImportService implements first-class personal configuration
 * import: paste/file → detect → parse → validate → capability
 * preview → redacted preview → Save, through the ONE parser / store /
 * capability architecture.
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.ImportService.";

/**
 * PreviewImport parses, validates and capability-resolves the payload
 * WITHOUT persisting anything. The returned preview is redacted (no
 * credentials) and reports which installed backends can execute each
 * configuration.
 * @param {string} payload
 * @returns {$CancellablePromise<import("./importtypes.js").ImportPreview>}
 */
export function PreviewImport(payload) {
    return $Call.ByName($prefix + "PreviewImport", payload);
}

/**
 * SaveImportedConfigs re-parses the payload and persists every
 * parseable configuration through the existing store (idempotent per
 * fingerprint — re-importing is an update, never a duplicate).
 * @param {string} payload
 * @returns {$CancellablePromise<import("./importtypes.js").ImportResult>}
 */
export function SaveImportedConfigs(payload) {
    return $Call.ByName($prefix + "SaveImportedConfigs", payload);
}
