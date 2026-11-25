package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The three held issues the hold reasons are read on: a task its owner stopped at testing, a
// parent whose plan has no acceptance step waiting before its retro for the issues beneath it,
// and an escalated hold, the one reason a caller below OwnerPlanContract knows. stoppedInMerge is
// a task stopped where it keeps no slot, and beneath an issue beneath childrenIssue, which the
// held rows of heldWordIssues name.
const (
	taskStoppedIssue = "LEGION-230"
	childrenIssue    = "LEGION-231"
	escalatedIssue   = "LEGION-232"
	stoppedInMerge   = "LEGION-233"
	beneath          = "LEGION-235"
)

// heldFor is key held from from, for reason.
func heldFor(key string, from phase.Phase, reason record.HoldReason) Issue {
	return Issue{Key: key, Generation: 1, Phase: phase.Held, Status: "in_progress", Tree: key, Repo: "acme/widgets",
		HoldReason: string(reason), HeldFrom: string(from)}
}

// heldIssues is the state golden with the three held issues beside its own.
func heldIssues(t *testing.T) State {
	t.Helper()
	state := populatedState(t)
	issues := make(map[string]Issue, len(state.Issues)+3)
	maps.Copy(issues, state.Issues)
	issues[taskStoppedIssue] = heldFor(taskStoppedIssue, phase.Testing, record.HoldTaskStopped)
	issues[childrenIssue] = heldFor(childrenIssue, phase.Retro, record.HoldForChildren)
	issues[escalatedIssue] = heldFor(escalatedIssue, phase.Implementing, record.HoldEscalated)
	state.Issues = issues
	return state
}

// heldWordIssues is heldIssues with a row of each held word OwnerPlanContract added after the
// golden's own rows: childrenIssue under children_before_retro, held by beneath, and
// stoppedInMerge under task_stopped, held by nothing.
func heldWordIssues(t *testing.T) State {
	t.Helper()
	state := heldIssues(t)
	state.Admission.Held = append(slices.Clone(state.Admission.Held),
		HeldIssue{Issue: childrenIssue, Reason: record.HeldChildrenBeforeRetro, HeldBy: []string{beneath}},
		HeldIssue{Issue: stoppedInMerge, Reason: record.HeldTaskStopped})
	return state
}

// holdsServed is each issue's holdReason and heldFrom as the body carries them, "" for a key the
// body leaves out.
func holdsServed(t *testing.T, body []byte) map[string][2]string {
	t.Helper()
	var served struct {
		Issues map[string]struct {
			HoldReason string `json:"holdReason"`
			HeldFrom   string `json:"heldFrom"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	holds := make(map[string][2]string, len(served.Issues))
	for key, issue := range served.Issues {
		holds[key] = [2]string{issue.HoldReason, issue.HeldFrom}
	}
	return holds
}

// recordHoldServed is the holdReason and heldFrom of one issue as the record route serves it to a
// caller stating contract, "" for a key the body leaves out.
func recordHoldServed(t *testing.T, handler http.Handler, key string, contract int) [2]string {
	t.Helper()
	recorder := recordAsked(t, handler, "192.0.2.1:41000", key, contract)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the record of %s answered %d to a caller at %d: %s", key, recorder.Code, contract, recorder.Body)
	}
	var served struct {
		HoldReason string `json:"holdReason"`
		HeldFrom   string `json:"heldFrom"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &served); err != nil {
		t.Fatalf("decode the record of %s %s: %v", key, recorder.Body, err)
	}
	return [2]string{served.HoldReason, served.HeldFrom}
}

// A caller whose plugin speaks a contract below OwnerPlanContract reads `holdReason` with the one
// value `escalated` and refuses the whole body for another, so it is served an issue held for
// task-stopped or children with no holdReason, as a hold of no reason is served; heldFrom stays,
// and so does every escalated hold. A caller at the contract is served the three reasons. The
// rule is the value strip's (withoutUnadmittedHoldReason), so it follows the contract the caller
// states, else its pod's lowest, and the record route serves each issue as the state does. The
// pod reads a contract below shapedAnswersContract, since a build from that contract on states
// its own and is in no pod's reckoning (podContracts). The state the source answered keeps the
// two reasons for the next caller.
func TestAnOlderCallerIsServedNoNewHoldReason(t *testing.T) {
	const remote = "192.0.2.1:41000"
	state := heldIssues(t)
	handler := stateServer(t, state, podClaims(DaemonAPIVersion()))
	newer := map[string][2]string{
		taskStoppedIssue: {"task-stopped", "testing"},
		childrenIssue:    {"children", "retro"},
		escalatedIssue:   {"escalated", "implementing"},
	}
	older := map[string][2]string{
		taskStoppedIssue: {"", "testing"},
		childrenIssue:    {"", "retro"},
		escalatedIssue:   {"escalated", "implementing"},
	}
	for _, tc := range []struct {
		name   string
		served map[string][2]string
		want   map[string][2]string
	}{
		{"a caller stating the contract", holdsServed(t, stateStating(t, handler, remote, OwnerPlanContract)), newer},
		{"a caller stating the contract before it", holdsServed(t, stateStating(t, handler, remote, OwnerPlanContract-1)), older},
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
		if got := recordHoldServed(t, handler, key, OwnerPlanContract); got != hold {
			t.Errorf("the record of %s is served to a caller at the contract held %q, want %q", key, got, hold)
		}
		if got := recordHoldServed(t, handler, key, OwnerPlanContract-1); got != older[key] {
			t.Errorf("the record of %s is served to a caller one contract behind held %q, want %q", key, got, older[key])
		}
	}
	if got := state.Issues[taskStoppedIssue].HoldReason; got != string(record.HoldTaskStopped) {
		t.Errorf("the state the source answered holds %s's reason as %q after the narrowing, want it kept", taskStoppedIssue, got)
	}
}

// heldRowsServed is each row of the body's `admission.held` by issue key, as the wire carries it,
// so a key the row leaves off is absent from its map and not an empty value.
func heldRowsServed(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()
	var served struct {
		Admission struct {
			Held []map[string]any `json:"held"`
		} `json:"admission"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	rows := make(map[string]map[string]any, len(served.Admission.Held))
	for _, row := range served.Admission.Held {
		rows[row["issue"].(string)] = row
	}
	return rows
}

// A caller whose plugin speaks a contract below OwnerPlanContract reads `admission.held`'s
// `reason` as a strict list of the eleven words before it, and refuses the whole body for
// another. So the rows of the two words that contract added, children_before_retro and
// task_stopped, are served to it as what it read for those issues before: unclassified, with no
// heldBy. Every other row is served as it stands, and a caller at the contract is served the two
// words with their heldBy. The rule is the value strip's (withoutUnadmittedHeldWords), so it
// follows the contract the caller states, else its pod's lowest; the eleven words arrived with
// the field and ride the band of heldContract, which follows the pod's highest, so a pod whose
// lowest contract is below heldContract and whose highest is above it is served the eleven words
// as they stand and still neither new word. The state the source answered keeps the two words
// for the next caller.
func TestAnOlderCallerReadsTheNewHeldWordsAsUnclassified(t *testing.T) {
	const remote = "192.0.2.1:41000"
	golden := len(heldIssues(t).Admission.Held)
	state := heldWordIssues(t)
	if golden == 0 {
		t.Fatal("the state golden holds no held row, so nothing here shows a row of another word served as it stands")
	}
	handler := stateServer(t, state, podClaims(DaemonAPIVersion()))

	at := heldRowsServed(t, stateStating(t, handler, remote, OwnerPlanContract))
	if row := at[childrenIssue]; row["reason"] != string(record.HeldChildrenBeforeRetro) || !reflect.DeepEqual(row["heldBy"], []any{beneath}) {
		t.Errorf("%s is served to a caller at the contract as %v, want children_before_retro held by %s", childrenIssue, row, beneath)
	}
	if row := at[stoppedInMerge]; row["reason"] != string(record.HeldTaskStopped) {
		t.Errorf("%s is served to a caller at the contract as %v, want task_stopped", stoppedInMerge, row)
	}

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"a caller stating the contract before it", stateStating(t, handler, remote, OwnerPlanContract-1)},
		{"a pod stating nothing at the contract before shapedAnswersContract",
			stateFrom(t, state, statePodAddr+":41000", podClaims(shapedAnswersContract-1))},
		{"a pod recording the floor and the contract before shapedAnswersContract",
			stateFrom(t, state, statePodAddr+":41000", mixedPodClaims(ServingFloor, shapedAnswersContract-1))},
	} {
		below := heldRowsServed(t, tc.body)
		for _, key := range []string{childrenIssue, stoppedInMerge} {
			if row := below[key]; !reflect.DeepEqual(row, map[string]any{"issue": key, "reason": string(record.HeldUnclassified)}) {
				t.Errorf("%s: %s is served as %v, want unclassified with no heldBy", tc.name, key, row)
			}
		}
		for _, row := range state.Admission.Held[:golden] {
			if served := below[row.Issue]; served["reason"] != string(row.Reason) {
				t.Errorf("%s: %s is served as %v, want its word %s as it stands", tc.name, row.Issue, served, row.Reason)
			}
		}
	}
	if got := state.Admission.Held[golden].Reason; got != record.HeldChildrenBeforeRetro {
		t.Errorf("the state the source answered holds %s's word as %q after the narrowing, want it kept", childrenIssue, got)
	}
}
