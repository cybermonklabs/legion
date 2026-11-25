package api

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// workHeldReason is the sentence of an issue the work hold holds, as the projection renders it.
const workHeldReason = "Its phase is implementing, and the work hold holds the work of its tree LEGION-208 until every gated " +
	"step stands open again: version 4 of its spec document (artifact-208) closed the design gate."

// workHeldIssue is an issue the work hold holds (`gates.hold: work`), with the answer the state
// gives it (WhyHeld).
func workHeldIssue() Issue {
	return Issue{Key: "LEGION-214", Generation: 4, Phase: phase.Implementing, Status: "in_progress", Tree: "LEGION-208",
		Repo: "acme/widgets", DefaultBranch: "main", Workers: map[claim.Role]PhaseView{},
		Why: &WhyView{State: WhyHeld, Reason: workHeldReason}}
}

// workHeldState is the state golden with the held issue beside every answer it already carries.
func workHeldState(t *testing.T) State {
	t.Helper()
	state := populatedState(t)
	issues := make(map[string]Issue, len(state.Issues)+1)
	maps.Copy(issues, state.Issues)
	issues["LEGION-214"] = workHeldIssue()
	state.Issues = issues
	return state
}

// whyBeforeTheWorkHold is every word `why.state` takes at the contracts before the work hold's:
// one word more and that reader refuses the whole state (packages/contracts/src/legion-api.ts's
// legionWhyView before contract 290).
var whyBeforeTheWorkHold = []WhyState{WhyClaimStopped, WhyWaiting, WhyBlocked, WhyUnlisted}

// H36, decision 16: a caller whose plugin is below the contract that added `why: held` reads the
// state with no issue's `why` of that word, and the daemon's log says it was taken; every other
// answer, of the held issue's neighbours and of every other key, is served as the source answered
// it. A plugin from shapedAnswersContract on states its contract in its header, and one before it
// states none and is placed by the claims of its pod, so a caller below the contract is read both
// ways: a plugin stating the contract before, and a pod whose claims record a contract below
// shapedAnswersContract. A caller stating the contract, a pod whose claims record none and a
// caller in no pod are served the word. A caller below the contract that added `why` at all reads
// no `why` of any issue, and no line of this contract, since there is nothing of it left to take.
// The record route serves the held issue as the state does, at the contract before and at the
// contract. The state carries no suspension source on any body a plugin reads
// (TestNoSessionFacingBodyCarriesSuspendedBy), so `hold` reaches no such caller either.
//
// The contract is pinned rather than bounded, as supervise.PodUpdateContract is
// (TestPodUpdateContractIsTheContractThatIntroducedTheOp): a renumber of version.go's entry that
// left workHoldContract behind would serve the word to the readers of the contracts between.
func TestWorkHoldStateKeepsOlderCallersReading(t *testing.T) {
	if workHoldContract != 290 || workHoldContract > DaemonAPIVersion {
		t.Fatalf("workHoldContract = %d, want 290, the contract whose entry in version.go added `why: held`, at or below DaemonAPIVersion %d",
			workHoldContract, DaemonAPIVersion)
	}
	full := issueViews(t, stateFrom(t, workHeldState(t), "192.0.2.1:41000", podClaims(shapedAnswersContract-1)))
	if held, _ := full["LEGION-214"]["why"].(map[string]any); held["state"] != string(WhyHeld) {
		t.Fatalf("the fixture's held issue carries %v; this test proves nothing", full["LEGION-214"])
	}
	for _, tc := range []struct {
		name   string
		remote string
		// claims is the contract the claims of the caller's pod record, and stated the contract
		// the caller states in its header, 0 for none.
		claims  int
		stated  int
		held    bool
		answers bool
		lines   []string
	}{
		{name: "a plugin stating the contract before", remote: statePodAddr + ":41000", claims: DaemonAPIVersion,
			stated: workHoldContract - 1, answers: true, lines: []string{droppedWhy}},
		{name: "a pod whose plugin states no contract", remote: statePodAddr + ":41000", claims: shapedAnswersContract - 1,
			answers: true, lines: []string{droppedWhy}},
		{name: "a pod below the contract that added why", remote: statePodAddr + ":41000", claims: whyAndLineFieldsContract - 1},
		{name: "a plugin stating the contract", remote: statePodAddr + ":41000", claims: DaemonAPIVersion, stated: workHoldContract,
			held: true, answers: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: 0, held: true, answers: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: shapedAnswersContract - 1, held: true, answers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, narrowings := stateNarrowedStating(t, workHeldState(t), tc.remote, podClaims(tc.claims), tc.stated)
			served := issueViews(t, body)
			for key, view := range served {
				why, carries := view["why"].(map[string]any)
				switch {
				case carries && !tc.answers:
					t.Errorf("%s carries why %v, which the reader before whyAndLineFieldsContract refuses", key, why)
				case carries && !tc.held && !slices.Contains(whyBeforeTheWorkHold, WhyState(why["state"].(string))):
					t.Errorf("%s carries why %v, a word the reader before %d refuses", key, why, workHoldContract)
				case tc.answers && key != "LEGION-214" && !reflect.DeepEqual(view["why"], full[key]["why"]):
					t.Errorf("%s's why is %v, want the source's %v: only the held answer is taken", key, view["why"], full[key]["why"])
				}
			}
			if got, want := served["LEGION-214"]["why"] != nil, tc.held; got != want {
				t.Errorf("the held issue carries why = %v, want %v: %v", got, want, served["LEGION-214"])
			}
			if tc.held && !reflect.DeepEqual(served["LEGION-214"]["why"], full["LEGION-214"]["why"]) {
				t.Errorf("the held issue's why is %v, want the source's %v", served["LEGION-214"]["why"], full["LEGION-214"]["why"])
			}
			var told []string
			for _, line := range narrowings {
				if line.Dropped == droppedWhy {
					told = append(told, line.Dropped)
				}
			}
			if !slices.Equal(told, tc.lines) {
				t.Errorf("the daemon's lines of this contract are %q, want %q: %+v", told, tc.lines, narrowings)
			}
		})
	}

	// The record route answers the issue the state holds for the same caller (shapeIssue): a caller
	// stating the contract before reads the held issue with no `why`, and one at the contract reads
	// the source's.
	records := stateServer(t, workHeldState(t), podClaims(DaemonAPIVersion))
	for _, contract := range []int{workHoldContract - 1, workHoldContract} {
		var record map[string]any
		if err := json.Unmarshal(recordOf(t, records, "192.0.2.1:41000", "LEGION-214", contract), &record); err != nil {
			t.Fatalf("decode the record of LEGION-214 at contract %d: %v", contract, err)
		}
		if held := contract >= workHoldContract; (record["why"] != nil) != held ||
			(held && !reflect.DeepEqual(record["why"], full["LEGION-214"]["why"])) {
			t.Errorf("the record of LEGION-214 at contract %d carries why %v, want it served %v, as the source's %v",
				contract, record["why"], held, full["LEGION-214"]["why"])
		}
	}

	// The narrowing writes a copy: the next caller of the same daemon, at the contract, is served
	// the answer the source gave.
	handler := stateServer(t, workHeldState(t), podClaims(DaemonAPIVersion))
	if served := issueViews(t, stateStating(t, handler, statePodAddr+":41000", workHoldContract-1)); served["LEGION-214"]["why"] != nil {
		t.Fatalf("the caller stating %d is served the held issue's why %v; the copy below proves nothing", workHoldContract-1, served["LEGION-214"]["why"])
	}
	again := issueViews(t, stateAsked(t, handler, "192.0.2.1:41000"))
	if !reflect.DeepEqual(again["LEGION-214"]["why"], full["LEGION-214"]["why"]) {
		t.Fatalf("after a narrowed answer the next caller is served %v, want the source's %v", again["LEGION-214"]["why"], full["LEGION-214"]["why"])
	}
}

// The state of an issue the work hold holds is written where the contracts suite parses it, so a
// word added to the Go side alone fails there (`legion-api.test.ts`'s fixture gate).
func TestStateWorkHoldGolden(t *testing.T) {
	golden(t, "state-work-hold.json", State{
		Daemon: DaemonInfo{
			Project:       "LEGION",
			SchemaVersion: 3,
			Boots:         5,
			FirstBootAt:   time.Date(2026, 11, 2, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 11, 20, 8, 30, 0, 0, time.UTC),
		},
		Admission:           Admission{Cap: 2, Active: []string{"LEGION-214"}, Waiting: []string{}, Trees: []string{"LEGION-208"}},
		Issues:              map[string]Issue{"LEGION-214": workHeldIssue()},
		PendingStatusWrites: []PendingStatusWrite{},
		Settings:            SettingsOf(minimalSettingsConfig(t)),
	})
}
