// @ts-check

/**
 * v0.12.2 — ProxyChain view models (JSDoc typedef module mirroring the
 * Go views in engine/app/proxychain.go). Credential-free: hop views
 * carry table-grade endpoint facts and REAL measured evidence, never
 * secrets.
 *
 * @module
 */

/**
 * One chain (list surface).
 * @typedef {object} ProxyChainView
 * @property {string} id
 * @property {string} name
 * @property {number} hops
 * @property {number=} created_at
 * @property {number=} updated_at
 */

/**
 * One hop with its REAL stored evidence.
 * @typedef {object} ProxyChainHopView
 * @property {number} position
 * @property {string} config_id
 * @property {string=} name
 * @property {string=} protocol
 * @property {string=} address
 * @property {number=} port
 * @property {string=} transport
 * @property {string=} security
 * @property {boolean} working
 * @property {number=} latency_ms
 * @property {number=} tested_at
 * @property {string=} test_backend
 * @property {boolean} available false = the configuration no longer
 * resolves in the store (the chain needs editing).
 */

/**
 * Full editor/connect projection.
 * @typedef {object} ProxyChainDetails
 * @property {string} id
 * @property {string} name
 * @property {string[]} config_ids
 * @property {ProxyChainHopView[]} hops
 * @property {string=} preview "A → B → C"
 * @property {number=} created_at
 * @property {number=} updated_at
 * @property {boolean} usable every hop currently resolves
 */

/**
 * Fresh end-to-end probe verdict.
 * @typedef {object} ChainE2EView
 * @property {boolean} ok
 * @property {number=} ping_ms
 * @property {boolean} measured
 * @property {string=} backend
 * @property {string=} quality
 * @property {string=} last_error
 * @property {number} at
 */

/**
 * Honest chain check: per-hop evidence + fresh end-to-end result.
 * @typedef {object} ChainCheckResult
 * @property {string} chain_id
 * @property {ProxyChainHopView[]} hops
 * @property {ChainE2EView} end_to_end
 * @property {number} duration_ms
 */

export {};
