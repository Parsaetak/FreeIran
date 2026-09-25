// @ts-check
// importtypes.js — hand-maintained mirror of the Go import preview
// structs (engine/app/importservice.go). Same discipline as
// profiletypes.js: field-for-field with the Go JSON tags.

/**
 * One redacted preview row: identity + capability truth, no
 * credentials.
 * @typedef {Object} ImportedConfigView
 * @property {number} index
 * @property {string} name
 * @property {string} protocol
 * @property {string} address
 * @property {number} port
 * @property {string} [transport]
 * @property {string} [security]
 * @property {string} redacted
 * @property {string[]} [backends]
 * @property {boolean} executable
 * @property {string} config_id
 * @property {string[]} [warnings]
 */

/**
 * One rejected entry with its reason.
 * @typedef {Object} ImportRejected
 * @property {number} index
 * @property {string} reason
 * @property {string} [snippet]
 */

/**
 * The full preview result.
 * @typedef {Object} ImportPreview
 * @property {string} format
 * @property {number} total
 * @property {ImportedConfigView[]} imported
 * @property {ImportRejected[]} rejected
 */

/**
 * What a save persisted.
 * @typedef {Object} ImportResult
 * @property {number} saved_count
 * @property {number} updated_count
 * @property {number} rejected_count
 * @property {string[]} config_ids
 * @property {string[]} [executable_ids]
 */

export {};
