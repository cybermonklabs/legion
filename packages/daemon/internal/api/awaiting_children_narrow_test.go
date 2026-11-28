package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// awaitingChildrenState is a state holding a parent in awaiting_children, held under that word,
// beside an issue at work and a held row of a word every reader of `admission.held` knows.
func awaitingChildrenState() State {
	return State{
		Issues: map[string]Issue{
			"LEGION-208": {Key: "LEGION-208", Phase: phase.AwaitingChildren, Status: "in_progress", Workers: map[claim.Role]PhaseView{}},
			"LEGION-209": {Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress", Workers: map[claim.Role]PhaseView{}},
		},
		Admission: Admission{Held: []HeldIssue{
			{Issue: "LEGION-208", Reason: record.HeldAwaitingChildren},
			{Issue: "LEGION-210", Reason: record.HeldDesignGate},
		}},
	}
}

// phasesAndHeldWords is what a state body carries of the two words gateBasisContract added:
// each issue's `phase`, by key, and each held row's `reason`, by the issue it names.
func phasesAndHeldWords(t *testing.T, body []byte) (phases, reasons map[string]string) {
	t.Helper()
	var served struct {
		Issues map[string]struct {
			Phase string `json:"phase"`
		} `json:"issues"`
		Admission struct {
			Held []struct {
				Issue  string `json:"issue"`
				Reason string `json:"reason"`
			} `json:"held"`
		} `json:"admission"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	phases, reasons = map[string]string{}, map[string]string{}
	for key, issue := range served.Issues {
		phases[key] = issue.Phase
	}
	for _, row := range served.Admission.Held {
		reasons[row.Issue] = row.Reason
	}
	return phases, reasons
}

// A caller below gateBasisContract knows neither awaiting_children word, so the value strips of
// that contract serve it the issue's phase as admitted and the held row's reason as
// children_first (dispatch://INFRA-3320 R13; shaping.go's withoutUnadmittedPhases and
// withoutUnadmittedHeldWords, whose heldReasonStandIn names the word). That holds for a caller
// that states a contract below it, which is served the nearest recorded contract's shape, and for
// one that states nothing from a pod whose claims record one, and on the record route as on the
// state, since the record answers the issue the state holds. A caller at the contract reads the
// new words, which is the state as the source holds it, and the source is left as it was for the
// next caller.
func TestAwaitingChildrenNarrowsToAdmittedAndChildrenFirstBelowItsContract(t *testing.T) {
	state := awaitingChildrenState()
	const operator = "192.0.2.1:41000"
	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		stated int
		phase  phase.Phase
		reason record.HeldReason
	}{
		{name: "a caller stating the contract just below", remote: operator, claims: podClaims(DaemonAPIVersion()), stated: entryNumber(gateBasisContract) - 1,
			phase: phase.Admitted, reason: record.HeldChildrenFirst},
		{name: "a pod at pauseContract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract),
			phase: phase.Admitted, reason: record.HeldChildrenFirst},
		{name: "a caller stating the contract", remote: operator, claims: podClaims(pauseContract), stated: entryNumber(gateBasisContract),
			phase: phase.AwaitingChildren, reason: record.HeldAwaitingChildren},
		{name: "a caller in no pod stating nothing", remote: operator, claims: podClaims(pauseContract),
			phase: phase.AwaitingChildren, reason: record.HeldAwaitingChildren},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := stateServer(t, state, tc.claims)
			phases, reasons := phasesAndHeldWords(t, stateStating(t, handler, tc.remote, tc.stated))
			if got := phases["LEGION-208"]; got != string(tc.phase) {
				t.Errorf("LEGION-208's phase is served as %q, want %q", got, tc.phase)
			}
			if got := phases["LEGION-209"]; got != string(phase.Implementing) {
				t.Errorf("LEGION-209's phase is served as %q, want implementing unchanged", got)
			}
			if got := reasons["LEGION-208"]; got != string(tc.reason) {
				t.Errorf("LEGION-208's held row is served the reason %q, want %q", got, tc.reason)
			}
			if got := reasons["LEGION-210"]; got != string(record.HeldDesignGate) {
				t.Errorf("LEGION-210's held row is served the reason %q, want design_gate unchanged", got)
			}

			recorded := asked(t, handler, tc.remote, "/legion/v1/issues/LEGION-208/record", tc.stated)
			if recorded.Code != http.StatusOK {
				t.Fatalf("the record of LEGION-208 = %d, want 200; body %s", recorded.Code, recorded.Body)
			}
			var issue struct {
				Phase string `json:"phase"`
			}
			if err := json.Unmarshal(recorded.Body.Bytes(), &issue); err != nil {
				t.Fatalf("decode the record %s: %v", recorded.Body, err)
			}
			if issue.Phase != string(tc.phase) {
				t.Errorf("the record of LEGION-208 serves the phase %q, want %q, the state's own reading of it", issue.Phase, tc.phase)
			}
		})
	}

	if got := state.Issues["LEGION-208"].Phase; got != phase.AwaitingChildren {
		t.Errorf("the source issue's phase = %s, want awaiting_children left as it was", got)
	}
	if got := state.Admission.Held[0].Reason; got != record.HeldAwaitingChildren {
		t.Errorf("the source held reason = %s, want awaiting_children left as it was", got)
	}
}
