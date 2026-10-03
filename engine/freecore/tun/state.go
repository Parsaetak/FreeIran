package tun

import (
	"net/netip"
	"time"
)

// DesiredState is what a first-party TUN activation wants the machine
// to look like. It is a pure value: the platform layer turns it into
// real adapter/address/route mutations, and the transaction records
// what was actually done so rollback can invert exactly that.
type DesiredState struct {
	Identity   Identity
	MTU        int
	Addresses  []netip.Prefix
	Routes     []netip.Prefix
	DNSServers []netip.Addr
}

// AppliedStep is one completed platform mutation together with its
// inverse. Undo functions must be idempotent: a rollback may retry
// them after a partial failure (the next boot's recovery replays the
// same plan).
type AppliedStep struct {
	Name string
	Undo func() error
}

// TransactionState is the explicit lifecycle of one activation.
type TransactionState string

const (
	// TransactionPending: steps recorded, not yet committed.
	TransactionPending TransactionState = "pending"

	// TransactionCommitted: the desired state is fully applied.
	TransactionCommitted TransactionState = "committed"

	// TransactionRolledBack: every recorded step was inverted.
	TransactionRolledBack TransactionState = "rolled_back"

	// TransactionRollbackIncomplete: rollback ran but at least one
	// undo failed — the residual is carried honestly, never hidden.
	TransactionRollbackIncomplete TransactionState = "rollback_incomplete"
)

// Transaction is the first-party TUN activation's state
// representation: the desired state, the steps actually applied and
// the resulting lifecycle state. The Phase 2 activation path feeds
// real undo closures; the representation and its ordering/error
// semantics are fixed and unit-tested here.
type Transaction struct {
	Desired DesiredState
	State   TransactionState

	steps []AppliedStep
}

// NewTransaction creates a pending transaction for a desired state.
func NewTransaction(desired DesiredState) *Transaction {
	return &Transaction{Desired: desired, State: TransactionPending}
}

// Record adds one applied step (with its inverse).
func (t *Transaction) Record(step AppliedStep) {
	t.steps = append(t.steps, step)
}

// Commit marks the transaction committed.
func (t *Transaction) Commit() {
	t.State = TransactionCommitted
}

// Steps returns the applied steps in application order.
func (t *Transaction) Steps() []AppliedStep {
	out := make([]AppliedStep, len(t.steps))
	copy(out, t.steps)

	return out
}

// Rollback inverts every applied step in REVERSE order (the only
// correct order: the inverse of "address added after adapter" is
// "address removed before adapter"). Idempotent undos make the
// rollback replayable by crash recovery. The returned errors list
// every undo that failed; the transaction state says whether the
// rollback was clean.
func (t *Transaction) Rollback() []error {
	var errs []error

	for i := len(t.steps) - 1; i >= 0; i-- {
		if err := t.steps[i].Undo(); err != nil {
			errs = append(errs, &RollbackStepError{Step: t.steps[i].Name, Err: err})
		}
	}

	if len(errs) > 0 {
		t.State = TransactionRollbackIncomplete
	} else {
		t.State = TransactionRolledBack
	}

	return errs
}

// RollbackStepError names the step whose undo failed.
type RollbackStepError struct {
	Step string
	Err  error
}

// Error implements error.
func (e *RollbackStepError) Error() string {
	return "tun rollback step " + e.Step + " failed: " + e.Err.Error()
}

// Unwrap exposes the underlying cause.
func (e *RollbackStepError) Unwrap() error { return e.Err }

// Marker is the durable session marker a first-party activation
// persists BEFORE touching the machine (the same contract the
// system-proxy ownership marker follows): a crash mid-activation
// leaves a recovery plan, never orphan state.
type Marker struct {
	Identity   Identity     `json:"identity"`
	Desired    DesiredState `json:"desired"`
	Applied    []string     `json:"applied"`
	Phase      string       `json:"phase"`
	RecordedAt time.Time    `json:"recorded_at"`
}

// NewMarker snapshots the transaction into its durable form.
func NewMarker(tx *Transaction, phase string) Marker {
	applied := make([]string, 0, len(tx.steps))

	for _, step := range tx.steps {
		applied = append(applied, step.Name)
	}

	return Marker{
		Identity:   tx.Desired.Identity,
		Desired:    tx.Desired,
		Applied:    applied,
		Phase:      phase,
		RecordedAt: time.Now().UTC(),
	}
}
