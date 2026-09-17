// @ts-check
// TestQueueService bindings.
//
// Hand-written v0.8 bindings (see coreservice.js for the rationale:
// stable Call.ByName path until the wails3 generator is available on
// a GUI toolchain host). Method names stay in sync with
// engine/app/v6_services.go.
/**
 * TestQueueService exposes the background configuration-testing
 * queue: enqueueing, cancellation, mode control and live statistics.
 * @module
 */

// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore: Unused imports
import { Call as $Call, CancellablePromise as $CancellablePromise } from "@wailsio/runtime";

const $prefix = "github.com/Parsaetak/FreeIran/engine/app.TestQueueService.";

/**
 * Enqueue schedules one configuration for testing.
 * @param {string} fingerprint
 * @param {string} protocol
 * @param {string[] | null} backends
 * @param {number} priority
 * @param {string} source
 * @returns {$CancellablePromise<number>}
 */
export function Enqueue(fingerprint, protocol, backends, priority, source) {
    return $Call.ByName($prefix + "Enqueue", fingerprint, protocol, backends, priority, source);
}

/**
 * EnqueueMany schedules a batch of configurations; returns the number
 * actually enqueued (duplicates are skipped).
 * @param {any[]} tasks
 * @returns {$CancellablePromise<number>}
 */
export function EnqueueMany(tasks) {
    return $Call.ByName($prefix + "EnqueueMany", tasks);
}

/**
 * Cancel cancels one task by ID.
 * @param {number} taskID
 * @returns {$CancellablePromise<void>}
 */
export function Cancel(taskID) {
    return $Call.ByName($prefix + "Cancel", taskID);
}

/**
 * CancelBySource cancels every pending task from one source; returns
 * the number cancelled.
 * @param {string} sourceID
 * @returns {$CancellablePromise<number>}
 */
export function CancelBySource(sourceID) {
    return $Call.ByName($prefix + "CancelBySource", sourceID);
}

/**
 * CancelAll cancels every pending task; returns the number cancelled.
 * @returns {$CancellablePromise<number>}
 */
export function CancelAll() {
    return $Call.ByName($prefix + "CancelAll");
}

/**
 * SetMode switches the testing mode (quick / balanced / thorough...).
 * @param {string} mode
 * @returns {$CancellablePromise<void>}
 */
export function SetMode(mode) {
    return $Call.ByName($prefix + "SetMode", mode);
}

/**
 * Stats returns live queue statistics.
 * @returns {$CancellablePromise<any>}
 */
export function Stats() {
    return $Call.ByName($prefix + "Stats");
}

/**
 * Snapshot returns the most recent tasks (bounded by limit).
 * @param {number} limit
 * @returns {$CancellablePromise<any[]>}
 */
export function Snapshot(limit) {
    return $Call.ByName($prefix + "Snapshot", limit);
}

/**
 * Drain waits (bounded) for the queue to empty.
 * @returns {$CancellablePromise<void>}
 */
export function Drain() {
    return $Call.ByName($prefix + "Drain");
}

/**
 * EnqueueByFilter scans the store and enqueues every configuration
 * matching the filter (v0.9.0 §5 bulk testing).
 * @param {{scope: string, fingerprints?: string[], protocol?: string, source?: string, limit?: number, priority?: number}} filter
 * @returns {$CancellablePromise<{enqueued: number, skipped: number, scope: string}>}
 */
export function EnqueueByFilter(filter) {
    return $Call.ByName($prefix + "EnqueueByFilter", filter);
}

/**
 * Pause suspends task pickup: queued tests stay pending while
 * in-flight tests finish (v0.9.7).
 * @returns {$CancellablePromise<void>}
 */
export function Pause() {
    return $Call.ByName($prefix + "Pause");
}

/**
 * Resume lifts a Pause.
 * @returns {$CancellablePromise<void>}
 */
export function Resume() {
    return $Call.ByName($prefix + "Resume");
}

/**
 * Paused reports whether the queue is paused.
 * @returns {$CancellablePromise<boolean>}
 */
export function Paused() {
    return $Call.ByName($prefix + "Paused");
}
