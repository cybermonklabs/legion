package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergeMarkState is a state carrying all four merge-mark keys: two issues marked for a jump, two
// live holds — one of a named repository and one of every repository the project serves — one
// merge a hold caught when its row last ran, and the newest jump of each of two repositories,
// one of them asked and the other refused. Every key is filled, because the strip below takes
// all four together and a key that was empty in the fixture would be passed by a strip that
// never ran.
func mergeMarkState() State {
	setAt := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	endsAt := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	return State{
		MergeJumps: []MergeJumpView{
			{Issue: "LEGION-208", Reason: "the release blocker", Session: "controller-2", SetAt: setAt},
			{Issue: "LEGION-210", Reason: "the revert", Session: "controller-2", SetAt: setAt.Add(time.Minute)},
		},
		MergeHolds: []MergeHoldView{
			{Paths: []string{"packages/daemon/", "docs/"}, Repository: "acme/widgets",
				Reason: "the release window", EndsAt: endsAt, Session: "controller-2", SetAt: setAt},
			{Paths: []string{"migrations/"}, Reason: "the migration lands by hand",
				EndsAt: endsAt.Add(time.Hour), Session: "controller-2", SetAt: setAt},
		},
		MergeHeld: []MergeHeldView{
			{Issue: "LEGION-211", DueAt: endsAt.Add(time.Minute)},
		},
		MergeJumpRecords: []MergeJumpRecordView{
			{Repository: "acme/widgets", Issue: "LEGION-207", Number: 412, Head: "c0ffee1", At: setAt,
				Reason: "the earlier blocker", EntriesTotal: 3, Entries: []MergeQueueEntryView{
					{Number: 401, Position: 1, State: "QUEUED", EnqueuedAt: setAt.Add(-time.Hour)},
					{Number: 402, Position: 2, Jump: true, State: "MERGEABLE", EnqueuedAt: setAt.Add(-30 * time.Minute)},
				}, Outcome: record.MergeJumpAsked},
			{Repository: "zeta/tools", Issue: "LEGION-209", Number: 77, Head: "deadbee", At: setAt,
				Reason: "the hotfix", EntriesUnknown: true, Outcome: record.MergeJumpRefused,
				Answer: "the merge queue is not enabled on this repository"},
		},
	}
}

// mergeMarksServed is the four merge-mark keys of a state body that carries them, by key name.
// The keys are read by name (MergeMarkFields) and one the body does not carry is in no entry,
// because whether a key is on the wire at all is what the strip is read by: a reader below
// mergeMarksContract is to be served no such key, whether its own schema would refuse one it
// does not name or quietly drop it.
func mergeMarksServed(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var state map[string]json.RawMessage
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	served := make(map[string]json.RawMessage, len(MergeMarkFields))
	for _, field := range MergeMarkFields {
		if raw, carried := state[field]; carried {
			served[field] = raw
		}
	}
	return served
}

// The state route serves the four merge-mark keys as the projection answered them, and each of
// them as an array and never null: a daemon holding no mark at all still writes every key, since
// the plugin's reader refuses null where it reads a list.
func TestTheMergeMarksAreServedOnTheState(t *testing.T) {
	body := stateFrom(t, mergeMarkState(), "192.0.2.1:41000", podClaims(DaemonAPIVersion))
	var served struct {
		MergeJumps       []MergeJumpView       `json:"mergeJumps"`
		MergeHolds       []MergeHoldView       `json:"mergeHolds"`
		MergeHeld        []MergeHeldView       `json:"mergeHeld"`
		MergeJumpRecords []MergeJumpRecordView `json:"mergeJumpRecords"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	marks := mergeMarkState()
	if len(served.MergeJumps) != len(marks.MergeJumps) || served.MergeJumps[0].Issue != "LEGION-208" ||
		served.MergeJumps[0].Reason != "the release blocker" || served.MergeJumps[0].Session != "controller-2" {
		t.Errorf("the state %s serves the jump marks %+v, want the fixture's", body, served.MergeJumps)
	}
	if len(served.MergeHolds) != len(marks.MergeHolds) || served.MergeHolds[1].Repository != "" ||
		len(served.MergeHolds[0].Paths) != 2 {
		t.Errorf("the state %s serves the holds %+v, want the fixture's, the second of every repository", body, served.MergeHolds)
	}
	if len(served.MergeHeld) != 1 || served.MergeHeld[0].Issue != "LEGION-211" {
		t.Errorf("the state %s serves the held merges %+v, want the fixture's one", body, served.MergeHeld)
	}
	if len(served.MergeJumpRecords) != 2 || len(served.MergeJumpRecords[0].Entries) != 2 ||
		served.MergeJumpRecords[0].Outcome != record.MergeJumpAsked ||
		served.MergeJumpRecords[1].Outcome != record.MergeJumpRefused {
		t.Errorf("the state %s serves the jump records %+v, want the fixture's two", body, served.MergeJumpRecords)
	}

	empty := mergeMarksServed(t, stateFrom(t, State{}, "192.0.2.1:41000", podClaims(DaemonAPIVersion)))
	for _, field := range MergeMarkFields {
		if string(empty[field]) != "[]" {
			t.Errorf("a state holding no mark serves %s = %s, want an empty array: the plugin's reader refuses null", field, empty[field])
		}
	}
}

// A plugin below mergeMarksContract is served none of the four keys: one built before
// sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body for a
// key it does not name, and one built after strips the key, so the strip is what keeps the keys
// from a caller that did not ask for them. The route authenticates nobody, so it knows that
// plugin by the pod the request came from. The keys are dropped and not emptied, because that
// strict reader refuses a key it does not know as surely when it is an empty array as when it
// names a mark.
func TestTheMergeMarksAreStrippedBelowTheirContract(t *testing.T) {
	behind := stateFrom(t, mergeMarkState(), statePodAddr+":4100", podClaims(mergeMarksContract-1))
	if served := mergeMarksServed(t, behind); len(served) != 0 {
		t.Errorf("the pod below %d is served the keys %v, want none: they are not its contract's",
			mergeMarksContract, served)
	}

	at := mergeMarksServed(t, stateFrom(t, mergeMarkState(), statePodAddr+":4100", podClaims(mergeMarksContract)))
	for _, field := range MergeMarkFields {
		if _, carried := at[field]; !carried {
			t.Errorf("the pod at %d is served no %s, want the mark: %s", mergeMarksContract, field, at)
		}
	}
}

// mergeMarksContract is the contract whose entry in version.go added the four keys, and this
// daemon's own contract is never below it: a caller at this contract is served the marks.
func TestMergeMarksContractIsTheContractThatAddedTheKeys(t *testing.T) {
	const addedMergeMarks = 86
	if mergeMarksContract != addedMergeMarks {
		t.Fatalf("mergeMarksContract = %d, want %d, the contract whose entry in version.go added the controller's merge marks",
			mergeMarksContract, addedMergeMarks)
	}
	if mergeMarksContract > DaemonAPIVersion {
		t.Fatalf("mergeMarksContract = %d, above DaemonAPIVersion %d: no caller could be served the marks",
			mergeMarksContract, DaemonAPIVersion)
	}
}
