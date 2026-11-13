package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// waitingOnIssueMember is an issue standing in the waiting line as a started issue that gave its
// admission slot back to wait on another issue: the returned part, `waiting_on_issue`, holding no
// slot, placed by the moment its row was taken. Every field of its member beside `kind` is a
// field whyAndLineFieldsContract added, so a strip of waitingOnIssueContract that took more than
// the one word shows here.
func waitingOnIssueMember() Issue {
	holds := false
	since := time.Date(2026, 11, 10, 15, 11, 32, 0, time.UTC)
	return Issue{Key: "LEGION-213", Phase: phase.Implementing, Status: "in_progress",
		WaitingLine: &WaitingLineView{Place: 3, Length: 3, Part: "returned", Kind: waitingOnIssueKind,
			PartPlace: 2, PartLength: 2, HoldsSlot: &holds, Since: &since,
			Reason: "It gave its admission slot back to wait on LEGION-7 at 2026-11-10T15:11:32Z."}}
}

// A caller below waitingOnIssueContract reads `kind` with the four words before it and refuses
// the whole body for a fifth, so it is served every member of that kind without its `kind`, and
// every other field of the member and every member of another kind as they are; a caller at the
// contract is served the word. The state the source answered keeps the word for the next caller.
func TestWaitingOnIssueKindIsServedFromItsContract(t *testing.T) {
	state := whyAndLineFieldsState()
	issues := make(map[string]Issue, len(state.Issues)+1)
	for key, issue := range state.Issues {
		issues[key] = issue
	}
	issues["LEGION-213"] = waitingOnIssueMember()
	state.Issues = issues
	full := lineFieldsOf(t, *state.Issues["LEGION-213"].WaitingLine)

	below := stateFrom(t, state, statePodAddr+":4100", podClaims(waitingOnIssueContract-1))
	served := waitingLineFieldsServed(t, below)
	want := make(map[string]string, len(full))
	for name, value := range full {
		if name != "kind" {
			want[name] = value
		}
	}
	if !reflect.DeepEqual(served["LEGION-213"], want) {
		t.Errorf("the pod at %d is served LEGION-213's %s as %v, want every field but kind, %v",
			waitingOnIssueContract-1, WaitingLineField, served["LEGION-213"], want)
	}
	for key, issue := range state.Issues {
		if key == "LEGION-213" || issue.WaitingLine == nil {
			continue
		}
		if member := lineFieldsOf(t, *issue.WaitingLine); !reflect.DeepEqual(served[key], member) {
			t.Errorf("the pod at %d is served %s's %s as %v, want it whole, %v: its kind is not this contract's",
				waitingOnIssueContract-1, key, WaitingLineField, served[key], member)
		}
	}
	if state.Issues["LEGION-213"].WaitingLine.Kind != waitingOnIssueKind {
		t.Errorf("the strip wrote through to the state the source answered: its member's kind is %q",
			state.Issues["LEGION-213"].WaitingLine.Kind)
	}

	at := stateFrom(t, state, statePodAddr+":4100", podClaims(waitingOnIssueContract))
	if got := waitingLineFieldsServed(t, at)["LEGION-213"]; !reflect.DeepEqual(got, full) {
		t.Errorf("the pod at %d is served LEGION-213's %s as %v, want it whole, %v",
			waitingOnIssueContract, WaitingLineField, got, full)
	}
}

// waitingOnIssueContract is the contract whose entry in version.go added the word, and this
// daemon's own contract is never below it; it stands above whyAndLineFieldsContract, which added
// `kind` itself, since a word cannot arrive before its field.
func TestWaitingOnIssueContractIsTheContractThatAddedIt(t *testing.T) {
	const addedWaitingOnIssue = 100
	if waitingOnIssueContract != addedWaitingOnIssue {
		t.Fatalf("waitingOnIssueContract = %d, want %d", waitingOnIssueContract, addedWaitingOnIssue)
	}
	if waitingOnIssueContract > DaemonAPIVersion {
		t.Fatalf("waitingOnIssueContract = %d, above DaemonAPIVersion %d", waitingOnIssueContract, DaemonAPIVersion)
	}
	if waitingOnIssueContract <= whyAndLineFieldsContract {
		t.Fatalf("waitingOnIssueContract = %d, not above whyAndLineFieldsContract %d", waitingOnIssueContract, whyAndLineFieldsContract)
	}
}
