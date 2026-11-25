package api

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A parent waiting on its open Dispatch children, read through the state route
// (dispatch://INFRA-3320 R13, and the state read of its check
// dispatch://INFRA-3320/artifact/verification-md#b-k13): a plugin of the contract before
// gateBasisContract, the contract that added awaiting_children, knows neither of its words, so a
// caller below that contract — one that states a contract below it, or one that states none from
// a pod whose claims record a contract below shapedAnswersContract, the newest a pod reading
// places (podContracts) — is served the parent at phase `admitted`, a phase that names no
// worker's, and its held row under `children_first`, the word a started parent's wait took over,
// with the children that hold it beside. A caller that states the contract, a pod whose claims
// record it, which no pod reading counts, and a caller in no pod — the operator's `legion state`
// — read the new words. At every contract the parent stands in
// `admission.blocked` naming its open child and in neither `admission.active` nor
// `admission.waiting`. The state is the one the projection writes for a parent the admission pass
// moved into the phase while another tree's issue holds the cap's one slot
// (internal/projection's TestAWaitingParentStandsInBlockedWithItsOpenChildrenAlone reads it from
// the records).
//
// The completion route's side of the return is staleTesterCompletionAfterTheReturn: the tester's
// completion of its round from before the wait, sent at the same head after the return, is
// refused HANDOFF_STALE_ENTRY, moves nothing and records nothing, and the completion of the
// return's task is taken.
func TestParentWaitsHoldsNoSlotAndComesBack(t *testing.T) {
	const (
		parent = "LEGION-300"
		child  = "LEGION-301"
		filler = "LEGION-390"
	)
	runs := true
	state := State{
		Issues: map[string]Issue{
			parent: {Key: parent, Generation: 1, Phase: phase.AwaitingChildren, Status: "in_progress", Tree: parent, TreeRuns: &runs,
				Why: &WhyView{State: WhyBlocked, Reason: "It waits on its open Dispatch children: LEGION-301."}},
			filler: {Key: filler, Generation: 1, Phase: phase.Implementing, Status: "in_progress", Tree: filler, TreeRuns: &runs},
		},
		Admission: Admission{Cap: 1, Active: []string{filler}, Waiting: []string{},
			Blocked: []BlockedIssue{{Issue: parent, BlockedBy: []string{child}}},
			Held:    []HeldIssue{{Issue: parent, Reason: record.HeldAwaitingChildren, HeldBy: []string{child}}}},
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		stated int
		phase  phase.Phase
		held   record.HeldReason
	}{
		{name: "a caller stating the contract just below", remote: "192.0.2.1:41000", claims: podClaims(DaemonAPIVersion()), stated: entryNumber(gateBasisContract) - 1,
			phase: phase.Admitted, held: record.HeldChildrenFirst},
		{name: "a pod at the newest contract a pod reading places", remote: statePodAddr + ":41000", claims: podClaims(shapedAnswersContract - 1),
			phase: phase.Admitted, held: record.HeldChildrenFirst},
		{name: "a caller stating the contract that added the phase", remote: "192.0.2.1:41000", claims: podClaims(shapedAnswersContract - 1), stated: entryNumber(gateBasisContract),
			phase: phase.AwaitingChildren, held: record.HeldAwaitingChildren},
		{name: "a pod at the contract that added the phase", remote: statePodAddr + ":41000", claims: podClaims(entryNumber(gateBasisContract)),
			phase: phase.AwaitingChildren, held: record.HeldAwaitingChildren},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(shapedAnswersContract - 1),
			phase: phase.AwaitingChildren, held: record.HeldAwaitingChildren},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateStating(t, stateServer(t, state, tc.claims), tc.remote, tc.stated)
			var served struct {
				Issues map[string]struct {
					Phase  phase.Phase `json:"phase"`
					Status string      `json:"status"`
				} `json:"issues"`
				Admission struct {
					Active  []string              `json:"active"`
					Waiting []string              `json:"waiting"`
					Blocked []record.BlockedIssue `json:"blocked"`
					Held    []record.HeldIssue    `json:"held"`
				} `json:"admission"`
			}
			if err := json.Unmarshal(body, &served); err != nil {
				t.Fatalf("decode the state %s: %v", body, err)
			}
			if view := served.Issues[parent]; view.Phase != tc.phase || view.Status != "in_progress" {
				t.Fatalf("%s is served at phase %s, status %s; want phase %s, in_progress", parent, view.Phase, view.Status, tc.phase)
			}
			if view := served.Issues[filler]; view.Phase != phase.Implementing {
				t.Fatalf("%s is served at phase %s, want implementing as it stands", filler, view.Phase)
			}
			if want := []record.HeldIssue{{Issue: parent, Reason: tc.held, HeldBy: []string{child}}}; !reflect.DeepEqual(served.Admission.Held, want) {
				t.Fatalf("admission.held is served as %+v, want %+v", served.Admission.Held, want)
			}
			if want := []record.BlockedIssue{{Issue: parent, BlockedBy: []string{child}}}; !reflect.DeepEqual(served.Admission.Blocked, want) {
				t.Fatalf("admission.blocked is served as %+v, want %+v", served.Admission.Blocked, want)
			}
			if slices.Contains(served.Admission.Active, parent) || slices.Contains(served.Admission.Waiting, parent) {
				t.Fatalf("admission.active = %v, admission.waiting = %v; want %s in neither", served.Admission.Active, served.Admission.Waiting, parent)
			}
		})
	}

	// The route narrows a copy: the state the source answered still holds the new words.
	if state.Issues[parent].Phase != phase.AwaitingChildren || state.Admission.Held[0].Reason != record.HeldAwaitingChildren {
		t.Fatalf("the source state = %+v, %+v; want it left as it was", state.Issues[parent], state.Admission.Held)
	}

	t.Run(staleCompletionAfterTheReturn, staleTesterCompletionAfterTheReturn)
}
