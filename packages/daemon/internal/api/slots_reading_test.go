package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// slotsReadingServed is what a served state body carries of the slots reading: whether it carries
// `admission.slots`, and how many issues carry a `rank`, a `rootRank`, and a slot with a `state`
// or a `since`.
func slotsReadingServed(t *testing.T, body []byte) (slots bool, ranked, stated int) {
	t.Helper()
	var state struct {
		Admission map[string]json.RawMessage `json:"admission"`
		Issues    map[string]struct {
			Rank     string `json:"rank"`
			RootRank string `json:"rootRank"`
			Slot     *struct {
				State string          `json:"state"`
				Since json.RawMessage `json:"since"`
			} `json:"slot"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("read the served state: %v", err)
	}
	_, slots = state.Admission["slots"]
	for _, issue := range state.Issues {
		if issue.Rank != "" || issue.RootRank != "" {
			ranked++
		}
		if issue.Slot != nil && (issue.Slot.State != "" || len(issue.Slot.Since) > 0) {
			stated++
		}
	}
	return slots, ranked, stated
}

// withoutSlotsReadingKeys takes off a decoded state body the five keys slotsReadingContract added:
// `admission.slots`, each issue's `rank` and `rootRank`, and each slot's `state` and `since`. It is
// what the tests that compare a narrowed body with the whole one take off the whole one for a
// caller below that contract.
func withoutSlotsReadingKeys(body map[string]any) {
	if admission, ok := body["admission"].(map[string]any); ok {
		delete(admission, "slots")
	}
	issues, _ := body["issues"].(map[string]any)
	for _, value := range issues {
		issue, ok := value.(map[string]any)
		if !ok {
			continue
		}
		delete(issue, "rank")
		delete(issue, "rootRank")
		if slot, ok := issue["slot"].(map[string]any); ok {
			delete(slot, "state")
			delete(slot, "since")
		}
	}
}

// A caller below slotsReadingContract is served the state without `admission.slots`, the issues'
// `rank` and `rootRank`, and the slots' `state` and `since`, which its plugin's reader does not
// name: one that states the contract before this one (a plugin at or above shapedAnswersContract
// states its contract, and its pod's claims place it no more), and one in a pod whose claims
// record a contract below shapedAnswersContract. A caller that states this contract, and a caller
// in no pod, are served them as the source answered them. The narrowing leaves the source's state
// whole, so the next caller is served it all.
func TestTheStateRouteServesACallerItsPluginContractsSlotsReading(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(supervise.PodUpdateContract-1))
	wantSlots, wantRanked, wantStated := slotsReadingServed(t, full)
	if !wantSlots || wantRanked == 0 || wantStated == 0 {
		t.Fatalf("the fixture's state carries slots %t, %d ranked issues and %d stated slots; this test proves nothing: %s",
			wantSlots, wantRanked, wantStated, full)
	}
	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		stated int
		served bool
	}{
		{name: "a caller that states the contract before this one", remote: statePodAddr + ":41000",
			claims: podClaims(DaemonAPIVersion()), stated: entryNumber(slotsReadingContract) - 1},
		{name: "a pod whose claims record the pause's contract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract)},
		{name: "a caller that states this contract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract),
			stated: entryNumber(slotsReadingContract), served: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(pauseContract), served: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateStating(t, stateServer(t, populatedState(t), tc.claims), tc.remote, tc.stated)
			slots, ranked, stated := slotsReadingServed(t, body)
			if tc.served {
				if !slots || ranked != wantRanked || stated != wantStated {
					t.Fatalf("the state carries slots %t, %d ranked issues and %d stated slots, want the source's %t, %d and %d: %s",
						slots, ranked, stated, wantSlots, wantRanked, wantStated, body)
				}
				return
			}
			if slots || ranked != 0 || stated != 0 {
				t.Fatalf("the state carries slots %t, %d ranked issues and %d stated slots, want none of them: %s", slots, ranked, stated, body)
			}
			if !strings.Contains(string(body), `"slot"`) {
				t.Fatalf("the narrowed state carries no slot at all, want each slot kept without its state and since: %s", body)
			}
		})
	}
	t.Run("the next caller is served the state whole", func(t *testing.T) {
		handler := stateServer(t, populatedState(t), podClaims(pauseContract))
		behind := stateAsked(t, handler, statePodAddr+":41000")
		operator := stateAsked(t, handler, "192.0.2.1:41000")
		if slots, _, _ := slotsReadingServed(t, behind); slots {
			t.Fatalf("the pod behind the contract was served the slots reading: %s", behind)
		}
		if slots, ranked, stated := slotsReadingServed(t, operator); !slots || ranked != wantRanked || stated != wantStated {
			t.Fatalf("the caller after it was served slots %t, %d ranked issues and %d stated slots, want the source's whole: %s",
				slots, ranked, stated, operator)
		}
	})
}

// The slots-reading entry has a served number, not just an id. entryNumber's fallback for
// a missing registration stands above the daemon's own contract.
func TestTheSlotsReadingContractIsTheContractThatAddedTheKeys(t *testing.T) {
	number := entryNumber(slotsReadingContract)
	if number <= LastNumberedContract || number > DaemonAPIVersion() {
		t.Fatalf("slots-reading entry number = %d, want above the frozen contracts and at most %d",
			number, DaemonAPIVersion())
	}
}
