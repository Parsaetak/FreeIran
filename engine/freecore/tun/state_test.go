package tun

import (
	"errors"
	"net/netip"
	"testing"
)

func TestRollbackReversesOrder(t *testing.T) {
	var order []string

	tx := NewTransaction(DesiredState{Identity: DefaultIdentity()})
	tx.Record(AppliedStep{Name: "adapter", Undo: func() error { order = append(order, "adapter"); return nil }})
	tx.Record(AppliedStep{Name: "address", Undo: func() error { order = append(order, "address"); return nil }})
	tx.Record(AppliedStep{Name: "routes", Undo: func() error { order = append(order, "routes"); return nil }})

	errs := tx.Rollback()

	if len(errs) != 0 {
		t.Fatalf("clean rollback reported errors: %v", errs)
	}

	if tx.State != TransactionRolledBack {
		t.Fatalf("state = %q, want rolled_back", tx.State)
	}

	want := []string{"routes", "address", "adapter"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("rollback order = %v, want %v (reverse of application)", order, want)
		}
	}
}

func TestRollbackCollectsErrors(t *testing.T) {
	boom := errors.New("boom")

	tx := NewTransaction(DesiredState{Identity: DefaultIdentity()})
	tx.Record(AppliedStep{Name: "ok-early", Undo: func() error { return nil }})
	tx.Record(AppliedStep{Name: "fails", Undo: func() error { return boom }})
	tx.Record(AppliedStep{Name: "ok-late", Undo: func() error { return nil }})

	errs := tx.Rollback()

	if len(errs) != 1 {
		t.Fatalf("rollback errors = %v, want exactly the failing step", errs)
	}

	var stepErr *RollbackStepError
	if !errors.As(errs[0], &stepErr) || stepErr.Step != "fails" || !errors.Is(errs[0], boom) {
		t.Fatalf("rollback error does not name the failing step: %v", errs[0])
	}

	if tx.State != TransactionRollbackIncomplete {
		t.Fatalf("state = %q, want rollback_incomplete (residual is honest)", tx.State)
	}
}

func TestCommitAndSteps(t *testing.T) {
	tx := NewTransaction(DesiredState{
		Identity:  DefaultIdentity(),
		Addresses: []netip.Prefix{netip.MustParsePrefix("172.19.0.2/32")},
	})

	tx.Record(AppliedStep{Name: "adapter", Undo: func() error { return nil }})
	tx.Commit()

	if tx.State != TransactionCommitted {
		t.Fatalf("state = %q, want committed", tx.State)
	}

	steps := tx.Steps()
	if len(steps) != 1 || steps[0].Name != "adapter" {
		t.Fatalf("steps = %v", steps)
	}

	// The returned slice is a copy: mutating it cannot corrupt the
	// transaction.
	steps[0].Name = "mutated"

	if tx.Steps()[0].Name != "adapter" {
		t.Fatal("Steps() must return a defensive copy")
	}
}

func TestMarkerSnapshot(t *testing.T) {
	tx := NewTransaction(DesiredState{Identity: DefaultIdentity()})
	tx.Record(AppliedStep{Name: "adapter", Undo: func() error { return nil }})

	marker := NewMarker(tx, "pending")

	if marker.Identity != DefaultIdentity() {
		t.Fatal("marker must carry the deterministic identity")
	}

	if len(marker.Applied) != 1 || marker.Applied[0] != "adapter" {
		t.Fatalf("marker applied steps = %v", marker.Applied)
	}

	if marker.RecordedAt.IsZero() {
		t.Fatal("marker must carry a timestamp")
	}
}
