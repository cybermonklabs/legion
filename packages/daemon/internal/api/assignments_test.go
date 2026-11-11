package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
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

// Every assignment that applies to an issue rides its record: the tree experiment its whole tree
// was drawn for and the issue experiment drawn for the issue itself, each with the arm, the mode
// and the instant of the draw. A row a hand-over settled carries an empty arm, an empty mode and
// no instant, which is how a reader tells a unit settled at no arm from a unit never drawn,
// which carries no row — and no key at all, since the wire never writes an empty list.
func TestTheStateShowsEveryIssueTheExperimentsItRunsUnder(t *testing.T) {
	body := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(DaemonAPIVersion))

	drawn := memoryDrawnAt.Format("2006-01-02T15:04:05Z")
	planned := planDrawnAt.Format("2006-01-02T15:04:05Z")
	for key, want := range map[string][]map[string]any{
		"LEGION-208": {
			{"experiment": "memory", "variant": "memory", "mode": "random", "drawnAt": drawn},
			{"experiment": "plan-length", "variant": "short", "mode": "random", "drawnAt": planned},
		},
		"LEGION-209": {
			{"experiment": "memory", "variant": "memory", "mode": "random", "drawnAt": drawn},
			{"experiment": "plan-length", "variant": "long", "mode": "random", "drawnAt": planned},
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
// the three memory fields of contract 49 stand beside it untouched: a pod at 49 through 59 reads
// its tree's memory group as it always has and no assignment, and a pod below 49 reads neither.
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
		{name: "a pod at this contract", contract: DaemonAPIVersion, memory: true, assigns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), statePodAddr+":41000", podClaims(tc.contract))
			if carried := strings.Contains(string(body), `"assignments"`); carried != tc.assigns {
				t.Errorf("the state carries an assignment = %v, want %v; body %s", carried, tc.assigns, body)
			}
			view := issueViews(t, body)["LEGION-208"]
			if tc.memory {
				if view["memoryGroup"] != "memory" || view["memoryDrawMode"] != "random" || view["memoryDrawnAt"] != "2026-09-20T11:04:05Z" {
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
	const addedAssignments = 60
	if assignmentsContract != addedAssignments {
		t.Fatalf("assignmentsContract = %d, want %d, the contract whose entry in version.go added each issue's experiment assignments",
			assignmentsContract, addedAssignments)
	}
	if assignmentsContract > DaemonAPIVersion {
		t.Fatalf("assignmentsContract = %d, above DaemonAPIVersion %d: no caller would ever be served the field",
			assignmentsContract, DaemonAPIVersion)
	}
}

// The assignments ride in the order the project's file declares the experiments, which is the
// order the settings name them in, and not the order the record keys its rows by: the record
// answers them in the experiments' names, and a file that declares `review-depth` before
// `memory` is served in that order. An experiment the file no longer names keeps its row and
// rides after every declared one.
func TestTheStateServesTheAssignmentsInTheProjectsDeclaredOrder(t *testing.T) {
	at := memoryDrawnAt
	state := State{Issues: map[string]Issue{"LEGION-208": {
		Key: "LEGION-208", Generation: 1, Phase: phase.Implementing, Status: "in_progress",
		Workers: map[claim.Role]PhaseView{},
		// As record.Store.AssignmentsFor answers them: by the experiments' names.
		Assignments: []AssignmentView{
			{Experiment: "dropped", Variant: "on", Mode: "random", DrawnAt: &at},
			{Experiment: "memory", Variant: "memory", Mode: "random", DrawnAt: &at},
			{Experiment: "review-depth", Variant: "deep", Mode: "on", DrawnAt: &at},
		},
	}}}
	declared := []config.Experiment{
		{Name: "review-depth", Mode: config.ExperimentOn, Unit: config.ExperimentTree, OnVariant: "deep",
			Variants: []config.Variant{{Name: "deep", Weight: 1}, {Name: "shallow", Weight: 1}}},
		{Name: "memory", Mode: config.ExperimentRandom, Unit: config.ExperimentTree,
			Variants: []config.Variant{{Name: "memory", Weight: 1}, {Name: "none", Weight: 1}}},
	}
	handler := NewServer("127.0.0.1", 8437, Options{
		Steps:       config.DesignGateRootIssues.Steps(),
		Experiments: declared,
		State:       fakeSource{state: state, claims: podClaims(DaemonAPIVersion)}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
		CallerPod: func(netip.Addr) (string, bool) { return "", false },
	}).Handler
	request := httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil)
	request.RemoteAddr = "192.0.2.1:41000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("state = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.Bytes()

	named := make([]string, 0, 3)
	for _, assignment := range assignmentsOf(t, body, "LEGION-208") {
		named = append(named, assignment["experiment"].(string))
	}
	if want := []string{"review-depth", "memory", "dropped"}; !reflect.DeepEqual(named, want) {
		t.Fatalf("the assignments ride as %v, want %v: the file's order, with the experiment it no longer names last", named, want)
	}
	// The order is the settings' own, which is the one order a reader of the body can line the
	// assignments up against.
	settings := make([]string, 0, len(declared))
	for _, experiment := range experimentsOf(declared) {
		settings = append(settings, experiment.Name)
	}
	if !reflect.DeepEqual(named[:len(settings)], settings) {
		t.Fatalf("the assignments ride as %v, want the settings' order %v", named, settings)
	}
}
