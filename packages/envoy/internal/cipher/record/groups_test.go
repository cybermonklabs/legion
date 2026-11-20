package record

import (
	"errors"
	"fmt"
	"testing"
)

// TestGroupOfReadsTheGroupForm: an approver of the group form answers its bare address, and an
// approver that is a person, a sentinel, or the prefix with no address answers none.
func TestGroupOfReadsTheGroupForm(t *testing.T) {
	if address, ok := GroupOf("group:approvers@example.com"); !ok || address != "approvers@example.com" {
		t.Fatalf(`GroupOf("group:approvers@example.com") = %q, %v; want "approvers@example.com", true`, address, ok)
	}
	for _, approver := range []string{"approvers@example.com", "group:", "shared", AnyoneApprover, ""} {
		if address, ok := GroupOf(approver); ok || address != "" {
			t.Errorf("GroupOf(%q) = %q, %v; want \"\", false", approver, address, ok)
		}
	}
}

// TestApproverNotAdmittedIsLapsedOnlyWhenStale: a decider a stale group cannot vouch for is a
// lapsed grant (ErrApproverNotAdmittedNow), a decider a fresh group does not list is not, and the
// typed error is still found inside the broken-chain error that wraps it.
func TestApproverNotAdmittedIsLapsedOnlyWhenStale(t *testing.T) {
	stale := ApproverNotAdmittedError{Group: "approvers@example.com", Login: "member@example.com", Fresh: false}
	if !errors.Is(stale, ErrApproverNotAdmittedNow) {
		t.Fatalf("errors.Is(%v, ErrApproverNotAdmittedNow) = false for a stale group, want true", stale)
	}
	fresh := ApproverNotAdmittedError{Group: "approvers@example.com", Login: "member@example.com", Fresh: true}
	if errors.Is(fresh, ErrApproverNotAdmittedNow) {
		t.Fatalf("errors.Is(%v, ErrApproverNotAdmittedNow) = true for a fresh group, want false", fresh)
	}
	wrapped := fmt.Errorf("%w: %w", ErrChainBroken, fresh)
	var typed ApproverNotAdmittedError
	if !errors.As(wrapped, &typed) || typed != fresh {
		t.Fatalf("errors.As(%v) = %+v; want %+v", wrapped, typed, fresh)
	}
	if !errors.Is(wrapped, ErrChainBroken) || errors.Is(wrapped, ErrApproverNotAdmittedNow) {
		t.Fatalf("%v: want ErrChainBroken and not ErrApproverNotAdmittedNow", wrapped)
	}
}
