// @ts-check
// Hand-maintained ByName binding for the v0.9.11 Connection Profiles
// service (P2 §18). The wails3 generator cannot run on the current
// host (no GUI toolchain); this file follows the documented contract
// format the bindings contract test enforces statically (the $prefix
// constant plus one $Call.ByName invocation per exported Go method,
// every name verified against the service). When the generator next
// runs on a GUI toolchain host, this module is replaced by the
// machine-generated equivalent.

/**
 * ProfileService exposes Connection Profiles: named, persistent sets
 * of connection preferences activated through the ONE settings path
 * (never a second configuration store, never a second networking
 * flow).
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.ProfileService.";

/**
 * List returns every profile in deterministic order with live
 * Active/Default flags and live configuration availability.
 * @returns {$CancellablePromise<import("./profiletypes.js").ProfileView[]>}
 */
export function List() {
    return $Call.ByName($prefix + "List");
}

/**
 * Create adds a profile (no credential fields exist on the payload).
 * @param {import("./profiletypes.js").ProfileSpec} spec
 * @returns {$CancellablePromise<import("./profiletypes.js").ProfileView>}
 */
export function Create(spec) {
    return $Call.ByName($prefix + "Create", spec);
}

/**
 * Update replaces the editable fields of one profile.
 * @param {string} profileID
 * @param {import("./profiletypes.js").ProfileSpec} spec
 * @returns {$CancellablePromise<import("./profiletypes.js").ProfileView>}
 */
export function Update(profileID, spec) {
    return $Call.ByName($prefix + "Update", profileID, spec);
}

/**
 * Rename renames one profile.
 * @param {string} profileID
 * @param {string} name
 * @returns {$CancellablePromise<import("./profiletypes.js").ProfileView>}
 */
export function Rename(profileID, name) {
    return $Call.ByName($prefix + "Rename", profileID, name);
}

/**
 * Duplicate copies one profile under a derived unique name.
 * @param {string} profileID
 * @returns {$CancellablePromise<import("./profiletypes.js").ProfileView>}
 */
export function Duplicate(profileID) {
    return $Call.ByName($prefix + "Duplicate", profileID);
}

/**
 * Delete removes one profile (configurations are never touched).
 * @param {string} profileID
 * @returns {$CancellablePromise<void>}
 */
export function Delete(profileID) {
    return $Call.ByName($prefix + "Delete", profileID);
}

/**
 * SetDefault marks one profile as the startup profile.
 * @param {string} profileID
 * @returns {$CancellablePromise<void>}
 */
export function SetDefault(profileID) {
    return $Call.ByName($prefix + "SetDefault", profileID);
}

/**
 * ClearDefault removes the startup-profile marker.
 * @returns {$CancellablePromise<void>}
 */
export function ClearDefault() {
    return $Call.ByName($prefix + "ClearDefault");
}

/**
 * Active returns the currently active profile (ok=false when none).
 * @returns {$CancellablePromise<[import("./profiletypes.js").ProfileView, boolean]>}
 */
export function Active() {
    return $Call.ByName($prefix + "Active");
}

/**
 * SetActive selects the active profile and applies its preferences
 * through the one settings path. Activation is a preference change —
 * the next connect still runs the existing verified engine flows.
 * @param {string} profileID
 * @returns {$CancellablePromise<import("./profiletypes.js").ProfileView>}
 */
export function SetActive(profileID) {
    return $Call.ByName($prefix + "SetActive", profileID);
}
