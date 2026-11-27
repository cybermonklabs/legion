package api

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The two holds before a retro that FixBesideReviewContract added: an issue held while a fix of
// its task runs, and an issue held until each check a CI job runs passes at its head. Both stand
// from the retro, which keeps no slot. Beside them, what holds each: the check and the head of
// the fix, and the two checks that have not passed at the head.
const (
	fixHeldIssue = "LEGION-250"
	ciHeldIssue  = "LEGION-251"
	fixHoldCheck = "ci / test (ubuntu-24.04)"
	fixHoldHead  = "c0de0000000000000000000000000000000000ff"
	ciHoldBuild  = "ci / build"
)

// fixAndCIHolds is the state golden with the two holds before a retro beside its own issues and an
// escalated hold, the one reason every reader knows. Each of the two has the held row this daemon
// writes for it: fix_running with the fix's check and head, and ci_at_head with the checks that
// have not passed at the head.
func fixAndCIHolds(t *testing.T) State {
	t.Helper()
	state := populatedState(t)
	issues := make(map[string]Issue, len(state.Issues)+3)
	maps.Copy(issues, state.Issues)
	issues[fixHeldIssue] = heldFor(fixHeldIssue, phase.Retro, record.HoldForFix)
	issues[ciHeldIssue] = heldFor(ciHeldIssue, phase.Retro, record.HoldForCI)
	issues[escalatedIssue] = heldFor(escalatedIssue, phase.Implementing, record.HoldEscalated)
	state.Issues = issues
	state.Admission.Held = append(slices.Clone(state.Admission.Held),
		HeldIssue{Issue: fixHeldIssue, Reason: record.HeldFixRunning, HeldBy: []string{fixHoldCheck, fixHoldHead}},
		HeldIssue{Issue: ciHeldIssue, Reason: record.HeldCIAtHead, HeldBy: []string{ciHoldBuild, fixHoldCheck}})
	return state
}

// A caller whose plugin speaks a contract below FixBesideReviewContract reads `holdReason` with
// a `z.enum` of `escalated`, `task-stopped` and `children`, and refuses the whole body for another
// word. So it is served an issue held for a fix or for CI with no holdReason, as a hold of no
// reason is served: heldFrom stays, and so does every escalated hold. A caller at the contract is
// served both reasons. The rule is the value strip's (withoutUnadmittedHoldReason), so it follows
// the contract the caller states, else its pod's lowest, and the record route serves each issue as
// the state does. The state the source answered keeps both reasons for the next caller.
//
// Each reason's arrival is read from the declared table (EnumeratedFields(), to which the
// fix-beside-review entry adds both reasons) at that entry's number,
// entryNumber(FixBesideReviewContract). A reason the table leaves out reads as arriving at this
// daemon's own contract (fieldArrivals.heldBack), which is the entry's number only while no entry
// ranks after it: from then on such a reason would be held back from a caller at the entry's
// number, whose reader admits it.
func TestAnOlderCallerIsServedNoFixHoldOrCIHoldReason(t *testing.T) {
	const remote = "192.0.2.1:41000"
	fix := entryNumber(FixBesideReviewContract)
	state := fixAndCIHolds(t)
	handler := stateServer(t, state, podClaims(DaemonAPIVersion()))
	newer := map[string][2]string{
		fixHeldIssue:   {"fix", "retro"},
		ciHeldIssue:    {"ci", "retro"},
		escalatedIssue: {"escalated", "implementing"},
	}
	older := map[string][2]string{
		fixHeldIssue:   {"", "retro"},
		ciHeldIssue:    {"", "retro"},
		escalatedIssue: {"escalated", "implementing"},
	}
	for _, tc := range []struct {
		name   string
		served map[string][2]string
		want   map[string][2]string
	}{
		{"a caller stating the contract", holdsServed(t, stateStating(t, handler, remote, fix)), newer},
		{"a caller stating the contract before it", holdsServed(t, stateStating(t, handler, remote, fix-1)), older},
		{"a pod stating nothing at the contract before shapedAnswersContract",
			holdsServed(t, stateFrom(t, state, statePodAddr+":41000", podClaims(shapedAnswersContract-1))), older},
	} {
		for key, hold := range tc.want {
			if tc.served[key] != hold {
				t.Errorf("%s: %s is served held %q, want %q", tc.name, key, tc.served[key], hold)
			}
		}
	}
	for key, hold := range newer {
		if got := recordHoldServed(t, handler, key, fix); got != hold {
			t.Errorf("the record of %s is served to a caller at the contract held %q, want %q", key, got, hold)
		}
		if got := recordHoldServed(t, handler, key, fix-1); got != older[key] {
			t.Errorf("the record of %s is served to a caller one contract behind held %q, want %q", key, got, older[key])
		}
	}
	if got := state.Issues[fixHeldIssue].HoldReason; got != string(record.HoldForFix) {
		t.Errorf("the state the source answered holds %s's reason as %q after the narrowing, want it kept", fixHeldIssue, got)
	}
	arrivals := arrivalsAt(issueKeyPath + "holdReason")
	for _, reason := range []record.HoldReason{record.HoldForFix, record.HoldForCI} {
		if arrival, declared := arrivals.values[string(reason)]; !declared || arrival != fix {
			t.Errorf("the declared table records the hold reason %q at %d (declared: %v), want the fix-beside-review entry's number (%d): "+
				"an undeclared reason arrives at this daemon's own contract, which the next entry moves past the readers that admit it",
				reason, arrival, declared, fix)
		}
	}
}

// A caller whose plugin speaks a contract below FixBesideReviewContract reads `admission.held`'s
// `reason` with a `z.enum` of the words before it, and refuses the whole body for
// another. So the rows of the two words that contract added, fix_running and ci_at_head, are
// served to it as what it read for those issues before: unclassified, with no heldBy. Every other
// row is served as it stands, and a caller at the contract is served both words with their
// heldBy. The rule is the value strip's (withoutUnadmittedHeldWords), so it follows the contract
// the caller states, else its pod's lowest. The state the source answered keeps both words for the
// next caller. Each word's arrival is read from the declared table (EnumeratedFields(), to which
// the fix-beside-review entry adds both words) at that entry's number, for the reason the hold
// reasons' arrivals are read above.
func TestAnOlderCallerReadsTheEndHoldWordsAsUnclassified(t *testing.T) {
	const remote = "192.0.2.1:41000"
	fix := entryNumber(FixBesideReviewContract)
	others := len(populatedState(t).Admission.Held)
	if others == 0 {
		t.Fatal("the state golden holds no held row, so nothing here shows a row of another word served as it stands")
	}
	state := fixAndCIHolds(t)
	handler := stateServer(t, state, podClaims(DaemonAPIVersion()))

	at := heldRowsServed(t, stateStating(t, handler, remote, fix))
	for key, want := range map[string]map[string]any{
		fixHeldIssue: {"issue": fixHeldIssue, "reason": string(record.HeldFixRunning), "heldBy": []any{fixHoldCheck, fixHoldHead}},
		ciHeldIssue:  {"issue": ciHeldIssue, "reason": string(record.HeldCIAtHead), "heldBy": []any{ciHoldBuild, fixHoldCheck}},
	} {
		if !reflect.DeepEqual(at[key], want) {
			t.Errorf("%s is served to a caller at the contract as %v, want %v", key, at[key], want)
		}
	}

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"a caller stating the contract before it", stateStating(t, handler, remote, fix-1)},
		{"a pod stating nothing at the contract before shapedAnswersContract",
			stateFrom(t, state, statePodAddr+":41000", podClaims(shapedAnswersContract-1))},
	} {
		below := heldRowsServed(t, tc.body)
		for _, key := range []string{fixHeldIssue, ciHeldIssue} {
			if row := below[key]; !reflect.DeepEqual(row, map[string]any{"issue": key, "reason": string(record.HeldUnclassified)}) {
				t.Errorf("%s: %s is served as %v, want unclassified with no heldBy", tc.name, key, row)
			}
		}
		for _, row := range state.Admission.Held[:others] {
			if served := below[row.Issue]; served["reason"] != string(row.Reason) {
				t.Errorf("%s: %s is served as %v, want its word %s as it stands", tc.name, row.Issue, served, row.Reason)
			}
		}
	}
	if got := state.Admission.Held[others].Reason; got != record.HeldFixRunning {
		t.Errorf("the state the source answered holds %s's word as %q after the narrowing, want it kept", fixHeldIssue, got)
	}
	arrivals := arrivalsAt("admission.held.*.reason")
	for _, word := range []record.HeldReason{record.HeldFixRunning, record.HeldCIAtHead} {
		if arrival, declared := arrivals.values[string(word)]; !declared || arrival != fix {
			t.Errorf("the declared table records the held word %q at %d (declared: %v), want the fix-beside-review entry's number (%d)",
				word, arrival, declared, fix)
		}
	}
}

// The state route's body for a caller whose plugin is at the contract before
// FixBesideReviewContract, asking from a pod whose claims record it and stating it: the state of
// fixAndCIHolds, each of the two holds before a retro served with no holdReason, and each of their
// held rows as unclassified with no heldBy, the row that reader read for those issues before.
// `packages/contracts/src/legion-api.test.ts` parses the body with a frozen copy of the hold
// reasons and the held list that reader knows, which is the reader the strip is written for.
func TestStateBeforeTheFixHoldsGolden(t *testing.T) {
	fix := entryNumber(FixBesideReviewContract)
	server := stateServer(t, fixAndCIHolds(t), podClaims(fix-1))
	body := stateStating(t, server, statePodAddr+":41000", fix-1)
	holds := holdsServed(t, body)
	for _, key := range []string{fixHeldIssue, ciHeldIssue} {
		if holds[key] != [2]string{"", "retro"} {
			t.Errorf("%s is served held %q to a caller before the contract, want no reason and heldFrom retro", key, holds[key])
		}
	}
	golden(t, "state-before-fix-holds.json", json.RawMessage(body))
}
