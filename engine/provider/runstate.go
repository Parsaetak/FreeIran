// runstate.go implements the ONE monotonic, generation-scoped
// run-state model shared by every provider engine (v0.9.12).
//
// THE INVARIANT (§4/§6 of the v0.9.12 closure): within a single
// runtime generation/run, the published lifecycle state must never
// move backward, and an event that does not belong to the current
// run must never mutate it at all.
//
// Why this exists: a provider's readiness evidence arrives from
// asynchronous sources — a log-line scanner draining the child's
// stdout, an endpoint probe, a health tick. Every one of those can
// deliver an event AFTER the run reached readiness, after a failed
// start, or even after Stop (an in-flight scanner line, a buffered
// pipe write). The pre-0.9.12 Tor engine let exactly such a late
// "Bootstrapped 45%" line overwrite an already-published
// Complete=true, regressing the published state from ready back to
// starting. runState makes that class of bug unrepresentable:
//
//   - GENERATION: every run gets a fresh generation number; every
//     event carries the generation it belongs to. owns() is the one
//     gate every asynchronous mutator must pass — a mismatched or
//     ended generation discards the event.
//   - MONOTONIC PROGRESS: observed bootstrap progress only moves
//     forward within a run; a late lower-% line refreshes nothing.
//   - IMMUTABLE VERDICT: publishReady() is a one-way transition
//     (complete=false → true) performed ONLY by the readiness
//     supervisor once the documented readiness contract is
//     satisfied; no later event can revoke it.
//   - SINGLE TERMINATION: end() closes the event gate for the run
//     forever (Stop, failed start, teardown) — a dead run cannot be
//     resurrected by a stale callback.
//
// The struct is NOT internally synchronized: the owning engine
// holds its engine mutex around every access (the same lock that
// protects the process/endpoint fields), so the snapshot semantics
// stay exactly as before — atomic, consistent, race-free.
package provider

import "time"

// runState is the monotonic per-run lifecycle state of one provider
// run. Zero value is a fresh, idle run.
type runState struct {
	// gen is this run's generation number (0 = no run yet).
	gen uint64

	// progress is the highest bootstrap progress observed for this
	// run (0-100). Monotonic within the run.
	progress int

	// tag is the latest bootstrap tag/summary for the highest
	// observed progress.
	tag string

	// endpointReady records readiness EVIDENCE: the local proxy
	// endpoint(s) were probed and accepted connections. Evidence is
	// never revoked and is not, by itself, the readiness verdict.
	endpointReady bool

	// complete is the readiness VERDICT, published exactly once per
	// run by the readiness supervisor. Immutable once true.
	complete bool

	// ended closes the event gate: the run was stopped or failed.
	// No event of any kind may mutate the run afterwards.
	ended bool

	// updatedAt is the time of the last OBSERVED mutation of this
	// run (begin, progress, endpoint evidence, verdict, end). It is
	// stored — never sampled at read time — so repeated snapshots of
	// an unchanged run stay semantically equal for the state
	// publisher's dedup boundary.
	updatedAt time.Time
}

// begin starts a fresh run: the caller has already bumped the
// engine's generation counter; everything observable about the run
// resets with it.
func (r *runState) begin(gen uint64) {
	*r = runState{gen: gen, updatedAt: time.Now().UTC()}
}

// end terminates the run: the event gate closes permanently.
func (r *runState) end() {
	if r.gen != 0 {
		r.ended = true
		r.updatedAt = time.Now().UTC()
	}
}

// owns reports whether an event tagged with the given generation may
// mutate this run. A stale event (older generation), a future event
// (impossible ordering), or any event after the run ended is
// discarded.
func (r *runState) owns(gen uint64) bool {
	return r.gen != 0 && gen == r.gen && !r.ended
}

// observeProgress folds one observed bootstrap progress value into
// the run. Progress is monotonic: a value lower than the highest
// already-observed progress is a LATE event (the scanner received it
// after a newer one) and must not regress the run's state, so it is
// dropped. A higher (or first) value updates progress and tag. The
// readiness verdict is not touched here — progress observation is
// the scanner's authority, the verdict belongs to the supervisor.
func (r *runState) observeProgress(progress int, tag string) {
	if progress < 0 || progress > 100 {
		return
	}

	if progress < r.progress {
		return // late lower-% event: never regress
	}

	r.progress = progress

	if tag != "" {
		r.tag = tag
	}

	r.updatedAt = time.Now().UTC()
}

// observeEndpointReady records endpoint evidence (the local proxy
// listener accepted a real connection). Once true it stays true for
// the run — evidence is never revoked.
func (r *runState) observeEndpointReady() {
	if !r.endpointReady {
		r.endpointReady = true
		r.updatedAt = time.Now().UTC()
	}
}

// ready reports whether the readiness verdict has been published.
func (r *runState) ready() bool {
	return r.complete
}

// markFailed ends the run with a terminal failure tag (the pre-0.9.12
// engines recorded Tag "failed" on the published bootstrap view).
// The event gate closes exactly like end() — a failed run cannot be
// resurrected by late events — but the last honestly observed
// progress stays visible as evidence for diagnostics.
func (r *runState) markFailed(tag string) {
	if r.gen == 0 {
		return
	}

	r.ended = true

	if tag != "" {
		r.tag = tag
	}

	r.updatedAt = time.Now().UTC()
}

// publishReady publishes the readiness verdict. One-way, idempotent:
// the first call wins, later calls are no-ops. Returns true when
// this call performed the transition (the supervisor uses that to
// publish the state change exactly once).
func (r *runState) publishReady() bool {
	if r.complete {
		return false
	}

	r.complete = true
	r.updatedAt = time.Now().UTC()

	return true
}

// snapshot renders the run's published view as BootstrapInfo — the
// credential-free surface the UI consumes. Complete mirrors the
// verdict, never the scanner's last line, so a late low-% event can
// no longer make Info() contradict State().
func (r *runState) snapshot() BootstrapInfo {
	if r.gen == 0 {
		return BootstrapInfo{}
	}

	return BootstrapInfo{
		Active:    !r.ended && !r.complete,
		Progress:  r.progress,
		Tag:       r.tag,
		Complete:  r.complete,
		UpdatedAt: r.updatedAt,
	}
}
