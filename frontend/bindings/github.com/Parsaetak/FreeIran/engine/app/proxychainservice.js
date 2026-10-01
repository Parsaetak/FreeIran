// @ts-check

/**
 * v0.12.2 — ProxyChainService (hand-maintained ByName binding; the
 * wails3 generator cannot run on the current host). The Go bindings
 * contract test (engine/app/bindings_contract_test.go) verifies every
 * ByName target against the real service methods.
 *
 * A proxy chain is an ordered list of EXISTING configuration IDs
 * (index 0 = first hop, last = egress) compiled into ONE core process
 * at connect time. A chain is NOT a remote source and carries no
 * credentials — ids only.
 *
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.ProxyChainService.";

/**
 * ListProxyChains returns every chain with hop counts.
 * @returns {$CancellablePromise<Array<{id: string, name: string, hops: number, created_at?: number, updated_at?: number}>>}
 */
export function ListProxyChains() {
    return $Call.ByName($prefix + "ListProxyChains");
}

/**
 * CreateProxyChain builds a chain from an ordered list of EXISTING
 * configuration ids and persists it atomically.
 *
 * @param {string} name
 * @param {string[]} configIDs
 * @returns {$CancellablePromise<{id: string, name: string, hops: number, created_at?: number, updated_at?: number}>}
 */
export function CreateProxyChain(name, configIDs) {
    return $Call.ByName($prefix + "CreateProxyChain", name, configIDs);
}

/**
 * RenameProxyChain renames one chain.
 * @param {string} chainID
 * @param {string} name
 * @returns {$CancellablePromise<void>}
 */
export function RenameProxyChain(chainID, name) {
    return $Call.ByName($prefix + "RenameProxyChain", chainID, name);
}

/**
 * DeleteProxyChain removes one chain (configurations untouched).
 * @param {string} chainID
 * @returns {$CancellablePromise<void>}
 */
export function DeleteProxyChain(chainID) {
    return $Call.ByName($prefix + "DeleteProxyChain", chainID);
}

/**
 * AddHop inserts a configuration at a 0-based position (-1 appends).
 * @param {string} chainID
 * @param {string} configID
 * @param {number} position
 * @returns {$CancellablePromise<void>}
 */
export function AddHop(chainID, configID, position) {
    return $Call.ByName($prefix + "AddHop", chainID, configID, position);
}

/**
 * RemoveHop removes one configuration from the chain (refuses below
 * two hops).
 * @param {string} chainID
 * @param {string} configID
 * @returns {$CancellablePromise<void>}
 */
export function RemoveHop(chainID, configID) {
    return $Call.ByName($prefix + "RemoveHop", chainID, configID);
}

/**
 * ReorderHop moves one hop to a new 0-based position (clamped).
 * @param {string} chainID
 * @param {string} configID
 * @param {number} position
 * @returns {$CancellablePromise<void>}
 */
export function ReorderHop(chainID, configID, position) {
    return $Call.ByName($prefix + "ReorderHop", chainID, configID, position);
}

/**
 * ProxyChainDetails renders the full editor/connect projection with
 * per-hop evidence and the compiled preview.
 * @param {string} chainID
 * @returns {$CancellablePromise<import("./proxychainmodels").ProxyChainDetails>}
 */
export function ProxyChainDetails(chainID) {
    return $Call.ByName($prefix + "ProxyChainDetails", chainID);
}

/**
 * ValidateProxyChain validates a candidate hop list before save.
 * @param {string[]} configIDs
 * @returns {$CancellablePromise<void>}
 */
export function ValidateProxyChain(configIDs) {
    return $Call.ByName($prefix + "ValidateProxyChain", configIDs);
}

/**
 * CheckChain runs the honest chain check: per-hop stored evidence plus
 * a FRESH end-to-end measurement through the compiled chain.
 * @param {string} chainID
 * @returns {$CancellablePromise<import("./proxychainmodels").ChainCheckResult>}
 */
export function CheckChain(chainID) {
    return $Call.ByName($prefix + "CheckChain", chainID);
}
