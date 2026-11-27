package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// assignmentsOf is the `assignments` of one issue of a state body, as the plugin's reader sees
// them: the list of objects, and nil where the issue carries no key at all.
func assignmentsOf(t *testing.T, body []byte, key string) []map[string]any {
	t.Helper()
	view, shown := issueViews(t, body)[key]
	if !shown {
		t.Fatalf("the state %s carries no issue %s", body, key)
	}
	raw, carried := view["assignments"]
	if !carried {
		return nil
	}
	rows, listed := raw.([]any)
	if !listed {
		t.Fatalf("issue %s carries assignments %v, want a list", key, raw)
	}
	assignments := make([]map[string]any, len(rows))
	for i, row := range rows {
		assignment, object := row.(map[string]any)
		if !object {
			t.Fatalf("issue %s carries the assignment %v, want an object", key, row)
		}
		assignments[i] = assignment
	}
	return assignments
}

// Every assignment that applies to an issue rides its record: the tree experiment its tree was
// drawn for, where the issue holds that draw, and the issue experiment drawn for the issue itself,
// each with the arm, the mode and the instant of the draw, and, for an experiment that sets how its
// issues are done, the group its arm set (`plan`). A row a hand-over settled carries an empty arm,
// an empty mode and no instant, which is how a reader tells a unit settled at no arm from a unit
// never drawn, which carries no row — and no key at all, since the wire never writes an empty list.
func TestTheStateShowsEveryIssueTheExperimentsItRunsUnder(t *testing.T) {
	body := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(DaemonAPIVersion()))

	drawn := drawnAt(t, "memory")
	planned := drawnAt(t, "plan-length")
	for key, want := range map[string][]map[string]any{
		"LEGION-208": {
			{"experiment": "memory", "variant": "memory", "mode": "random", "drawnAt": drawn},
			{"experiment": "plan-length", "variant": "short", "mode": "random", "drawnAt": planned},
		},
		// A child of the drawn tree held at admitted: it has not started, so it took no tree draw
		// and runs under its own issue draws alone (R17 of dispatch://INFRA-3320).
		"LEGION-209": {
			{"experiment": "plan-length", "variant": "long", "mode": "random", "drawnAt": planned},
			{"experiment": "planning", "variant": "b", "mode": "random", "drawnAt": planned, "plan": "default"},
		},
		// A child of the drawn tree that has started: its first start took the root's draw as a row
		// of its own, which it runs under beside its own issue draws.
		"LEGION-220": {
			{"experiment": "memory", "variant": "memory", "mode": "random", "drawnAt": drawn},
			{"experiment": "plan-length", "variant": "short", "mode": "random", "drawnAt": planned},
			{"experiment": "planning", "variant": "a", "mode": "random", "drawnAt": planned},
		},
		// The hand-over's answer: an arm and a mode on the wire, both empty, and no instant.
		"LEGION-210": {{"experiment": "plan-length", "variant": "", "mode": ""}},
		// An issue of a tree no experiment has reached carries no key.
		"LEGION-211": nil,
	} {
		if got := assignmentsOf(t, body, key); !reflect.DeepEqual(got, want) {
			t.Errorf("issue %s of %s runs under %v, want %v", key, body, got, want)
		}
	}
}

// A caller below the contract that added the assignments is served the state without them, and
// the three memory fields of contract 49 stand beside it untouched: a pod from memoryContract to
// below assignmentsContract reads its tree's memory group as it always has and no assignment, and
// a pod below 49 reads neither.
// The gates are two, so one does not answer for the other.
func TestTheStateServedBelowTheAssignmentsContractKeepsTheMemoryFields(t *testing.T) {
	for _, tc := range []struct {
		name            string
		contract        int
		memory, assigns bool
	}{
		{name: "a pod a contract below the assignments", contract: assignmentsContract - 1, memory: true},
		{name: "a pod at the memory contract", contract: memoryContract, memory: true},
		{name: "a pod below the memory contract", contract: memoryContract - 1},
		{name: "a pod at this contract", contract: DaemonAPIVersion(), memory: true, assigns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), statePodAddr+":41000", podClaims(tc.contract))
			if carried := strings.Contains(string(body), `"assignments"`); carried != tc.assigns {
				t.Errorf("the state carries an assignment = %v, want %v; body %s", carried, tc.assigns, body)
			}
			view := issueViews(t, body)["LEGION-208"]
			if tc.memory {
				if view["memoryGroup"] != "memory" || view["memoryDrawMode"] != "random" || view["memoryDrawnAt"] != drawnAt(t, "memory") {
					t.Errorf("the drawn root's memory fields = %v, %v, %v; want the fixture's memory, random and its instant, whatever the assignments gate serves",
						view["memoryGroup"], view["memoryDrawMode"], view["memoryDrawnAt"])
				}
				return
			}
			for _, key := range []string{"memoryGroup", "memoryDrawMode", "memoryDrawnAt"} {
				if view[key] != nil {
					t.Errorf("the drawn root carries %s = %v below the memory contract, want none of the three", key, view[key])
				}
			}
		})
	}
}

// assignmentsContract is the contract whose entry in version.go added the field, and this
// daemon's own contract is never below it: a caller at this contract is served the assignments.
func TestTheAssignmentsContractIsTheContractThatAddedTheField(t *testing.T) {
	const addedAssignments = 141
	if assignmentsContract != addedAssignments {
		t.Fatalf("assignmentsContract = %d, want %d, the contract whose entry in version.go added each issue's experiment assignments",
			assignmentsContract, addedAssignments)
	}
	if assignmentsContract > DaemonAPIVersion() {
		t.Fatalf("assignmentsContract = %d, above DaemonAPIVersion %d: no caller would ever be served the field",
			assignmentsContract, DaemonAPIVersion())
	}
}

// A caller below the contract that added each assignment's group is served every assignment
// without `plan`, and the rest of each row as it stands; a caller at this daemon's contract is
// served the group. The fixture's child LEGION-209 runs under `planning`, whose arm set
// `default`. A caller at or above shapedAnswersContract states its contract in the header, which
// is what places it (no pod's claims place a caller there, callerKind.reaches), and a pod's claims
// place a caller at the assignments contract.
func TestTheStateServedBelowTheGroupContractCarriesNoPlan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims []supervise.Claim
		stated int
		plan   bool
	}{
		{name: "a caller stating the contract below the group", claims: podClaims(0), stated: assignmentPlanContract - 1},
		{name: "a pod at the assignments contract", claims: podClaims(assignmentsContract)},
		{name: "a caller stating this contract", claims: podClaims(0), stated: DaemonAPIVersion(), plan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateStating(t, stateServer(t, populatedState(t), tc.claims), statePodAddr+":41000", tc.stated)
			var planning map[string]any
			for _, row := range assignmentsOf(t, body, "LEGION-209") {
				if row["experiment"] == "planning" {
					planning = row
				}
			}
			if planning == nil || planning["variant"] != "b" || planning["mode"] != "random" {
				t.Fatalf("LEGION-209 runs under %v, want the planning row with its arm b and its mode at every contract from the assignments' on",
					assignmentsOf(t, body, "LEGION-209"))
			}
			if plan, carried := planning["plan"]; carried != tc.plan || (carried && plan != "default") {
				t.Errorf("the planning row carries plan %v (%v), want it carried = %v, as default", plan, carried, tc.plan)
			}
		})
	}
}

// assignmentPlanContract is the contract whose entry in version.go added the key, and this
// daemon's own contract is never below it: a caller at this contract is served the group.
func TestTheAssignmentPlanContractIsTheContractThatAddedTheKey(t *testing.T) {
	const addedPlan = 220
	if assignmentPlanContract != addedPlan {
		t.Fatalf("assignmentPlanContract = %d, want %d, the contract whose entry in version.go added each assignment's plan",
			assignmentPlanContract, addedPlan)
	}
	if assignmentPlanContract > DaemonAPIVersion() {
		t.Fatalf("assignmentPlanContract = %d, above DaemonAPIVersion %d: no caller would ever be served the key",
			assignmentPlanContract, DaemonAPIVersion())
	}
}
