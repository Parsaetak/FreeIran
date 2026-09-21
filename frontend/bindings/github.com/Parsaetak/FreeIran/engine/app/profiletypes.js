// @ts-check
// Hand-maintained model types for the v0.9.11 Connection Profiles
// service — they mirror the Go structs in engine/app/profiles.go
// field-for-field (JSON tags). When the wails3 generator next runs on
// a GUI toolchain host these become generated models.

/**
 * One connection profile (credential-free UI projection).
 * @typedef {Object} ProfileView
 * @property {string} id
 * @property {string} name
 * @property {"auto" | "configs" | "tor" | "psiphon"} mode
 * @property {string=} config_id
 * @property {string=} config_name
 * @property {boolean} config_available
 * @property {string=} preferred_backend
 * @property {number=} local_socks_port
 * @property {number=} local_http_port
 * @property {boolean | null=} auto_recovery
 * @property {boolean} active
 * @property {boolean} default
 * @property {number=} created_at
 */

/**
 * Create/update payload (no credential fields).
 * @typedef {Object} ProfileSpec
 * @property {string} name
 * @property {"auto" | "configs" | "tor" | "psiphon"} mode
 * @property {string=} config_id
 * @property {string=} preferred_backend
 * @property {number=} local_socks_port
 * @property {number=} local_http_port
 * @property {boolean | null=} auto_recovery
 */

export {};
