package legion

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAnIssuesRolesAreTheKnownRolesInTheirOrderThenTheRestByName: Roles lists every claim with its
// state, a `retired` one and a role legionclaim.Roles does not name included, the known roles in
// that list's order (planner before implementer, which no sort by name gives) and the rest after
// them by name. A claim written `null`, the sampler's word for a claim gone, and a claim whose
// value is no string are no claim. Working is the roles whose claim is `working`, by name, which
// is the board card's order.
func TestAnIssuesRolesAreTheKnownRolesInTheirOrderThenTheRestByName(t *testing.T) {
	var empty IssueState
	if roles, working := empty.Roles(), empty.Working(); roles == nil || len(roles) != 0 || working == nil || len(working) != 0 {
		t.Errorf("an issue with no claim has roles %#v and roles in a turn %#v, want two empty lists, never nil", roles, working)
	}

	var state IssueState
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for _, claim := range []struct{ role, value string }{
		{"zeta", `"idle"`},
		{"tester", `"idle"`},
		{"auditor", `"working"`},
		{"implementer", `"working"`},
		{"merger", `"idle"`},
		{"architect", `"retired"`},
		{"planner", `"working"`},
		{"reviewer", `"working"`},
		{"reviewer", `null`},
		{"designer", `5`},
	} {
		state.Observe(ClaimFieldPrefix+claim.role, []byte(claim.value), at)
	}
	wantRoles := []RoleClaim{
		{"architect", "retired"}, {"planner", "working"}, {"implementer", "working"}, {"tester", "idle"},
		{"merger", "idle"}, {"auditor", "working"}, {"zeta", "idle"},
	}
	if got := state.Roles(); !slices.Equal(got, wantRoles) {
		t.Errorf("the roles are %v, want %v", got, wantRoles)
	}
	if got, want := state.Working(), []string{"auditor", "implementer", "planner"}; !slices.Equal(got, want) {
		t.Errorf("the roles in a turn are %v, want %v", got, want)
	}
}

// TestAPlaceInTheWaitingLineCountsFromOneAndTheBlockersKeepTheDaemonsOrder: Place is an issue's
// index in admission.waiting plus one with the line's length, and no place for an issue the line
// does not name; BlockersOf is the stored entry's issues in the order the daemon stored them,
// which is not their sorted order here. A nil Admission, a project whose daemon recorded none,
// places nothing and blocks nothing.
func TestAPlaceInTheWaitingLineCountsFromOneAndTheBlockersKeepTheDaemonsOrder(t *testing.T) {
	admission := &Admission{
		Waiting: []string{"INFRA-3", "INFRA-16", "INFRA-2", "INFRA-17", "INFRA-18"},
		Blocked: []BlockedIssue{{Issue: "INFRA-4", BlockedBy: []string{"INFRA-9", "INFRA-7"}}, {Issue: "INFRA-5"}},
	}
	var none *Admission
	for _, row := range []struct {
		what          string
		admission     *Admission
		key           string
		place, length int
		ok            bool
	}{
		{"the first in the line", admission, "INFRA-3", 1, 5, true},
		{"the third in the line", admission, "INFRA-2", 3, 5, true},
		{"the last in the line", admission, "INFRA-18", 5, 5, true},
		{"an issue the line does not name", admission, "INFRA-4", 0, 0, false},
		{"an issue of a project with no admission block", none, "INFRA-3", 0, 0, false},
	} {
		place, length, ok := row.admission.Place(row.key)
		if place != row.place || length != row.length || ok != row.ok {
			t.Errorf("%s, %s: place %d of %d (%t), want %d of %d (%t)", row.what, row.key, place, length, ok, row.place, row.length, row.ok)
		}
	}
	if got, want := admission.BlockersOf("INFRA-4"), []string{"INFRA-9", "INFRA-7"}; !slices.Equal(got, want) {
		t.Errorf("INFRA-4's blockers are %v, want %v, in the order the daemon stored them", got, want)
	}
	for _, key := range []string{"INFRA-5", "INFRA-3"} {
		if got := admission.BlockersOf(key); len(got) != 0 {
			t.Errorf("%s's blockers are %v, want none", key, got)
		}
	}
	if got := none.BlockersOf("INFRA-4"); len(got) != 0 || got == nil {
		t.Errorf("with no admission block INFRA-4's blockers are %#v, want an empty list, not nil", got)
	}
}

// TestReadIssueReadsOneIssueItsProjectsAdmissionAndWhetherItsPhaseWasSeenToStart: ReadIssue answers
// one issue's newest observations and nothing of another issue, its project's admission block and
// no other project's, and whether a `phase` row older than the newest holds a value distinct from
// the newest one's, a `null` counting as a value. Two equal rows in a row, which the sampler never
// writes, are no start: a probe that asked only whether an older row exists would call that one
// seen. An issue whose newest phase is `null`, or that has no row, is not recorded, and an
// admission block written `null` is none.
func TestReadIssueReadsOneIssueItsProjectsAdmissionAndWhetherItsPhaseWasSeenToStart(t *testing.T) {
	database := storetest.Open(t)
	seedLegionProject(t, database, "INFRA", "http://127.0.0.1:1")
	seedLegionProject(t, database, "OTHER", "http://127.0.0.1:1")
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	hour := func(n int) time.Time { return at.Add(time.Duration(n) * time.Hour) }
	observe := func(project, issue, field, value string, when time.Time) {
		t.Helper()
		if _, err := database.Pool.Exec(t.Context(), `
			insert into legion_observations (project, issue_key, field, value, observed_at)
			values ($1, $2, $3, $4::jsonb, $5)
		`, project, issue, field, value, when); err != nil {
			t.Fatalf("record %s's %s %s: %v", issue, field, value, err)
		}
	}
	wait := `{"kind":"ask","ask":"6f1c0f8e-3d4b-4a8e-9f51-2b7c1d9e0a11","since":"2026-11-20T08:30:00Z"}`
	observe("INFRA", "INFRA-1", FieldPhase, `"awaiting_person"`, hour(1))
	observe("INFRA", "INFRA-1", FieldSlot, `0`, hour(1))
	observe("INFRA", "INFRA-1", FieldWaitingOn, wait, hour(1))
	observe("INFRA", "INFRA-1", FieldTree, `"INFRA-1"`, hour(1))
	observe("INFRA", "INFRA-1", ClaimFieldPrefix+"implementer", `"idle"`, hour(1))
	observe("INFRA", "INFRA-1", ClaimFieldPrefix+"reviewer", `"working"`, hour(1))
	observe("INFRA", "INFRA-1", ClaimFieldPrefix+"reviewer", `null`, hour(2))
	observe("INFRA", "INFRA-2", FieldPhase, `"planning"`, hour(1))
	observe("INFRA", "INFRA-2", FieldPhase, `"implementing"`, hour(2))
	observe("INFRA", "INFRA-2", ClaimFieldPrefix+"tester", `"working"`, hour(2))
	observe("INFRA", "INFRA-3", FieldPhase, `"planning"`, hour(1))
	observe("INFRA", "INFRA-3", FieldPhase, `null`, hour(2))
	observe("INFRA", "INFRA-3", FieldPhase, `"planning"`, hour(3))
	observe("INFRA", "INFRA-4", FieldPhase, `"planning"`, hour(1))
	observe("INFRA", "INFRA-4", FieldPhase, `"planning"`, hour(2))
	observe("INFRA", "INFRA-5", FieldPhase, `"reviewing"`, hour(1))
	observe("INFRA", "INFRA-5", FieldPhase, `null`, hour(2))
	observe("INFRA", AdmissionKey, FieldAdmission,
		`{"cap": 2, "waiting": ["INFRA-3", "INFRA-1"], "blocked": [{"issue": "INFRA-1", "blockedBy": ["INFRA-9", "INFRA-7"]}]}`, hour(1))
	observe("INFRA", AdmissionKey, FieldSettings, `{"admission_cap": 2}`, hour(1))
	observe("OTHER", "OTHER-1", FieldPhase, `"testing"`, hour(1))
	observe("OTHER", AdmissionKey, FieldAdmission, `{"waiting": ["OTHER-1"]}`, hour(1))
	observe("OTHER", AdmissionKey, FieldAdmission, `null`, hour(2))

	read := func(project, key string) IssueCurrent {
		t.Helper()
		current, err := ReadIssue(t.Context(), database.Pool, project, key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		return current
	}

	first := read("INFRA", "INFRA-1")
	if !first.Recorded || first.PhaseStartSeen || first.State.Phase != PhaseAwaitingPerson || !first.State.PhaseSince.Equal(hour(1)) {
		t.Errorf("INFRA-1 is recorded %t in %q since %s, seen to start %t; want recorded in awaiting_person since %s, not seen to start: its phase is its first row",
			first.Recorded, first.State.Phase, first.State.PhaseSince, first.PhaseStartSeen, hour(1))
	}
	if slot := first.State.Slot; slot == nil || *slot != 0 {
		t.Errorf("INFRA-1's slot is %v, want 0", slot)
	}
	if got, want := first.State.Roles(), []RoleClaim{{"implementer", "idle"}}; !slices.Equal(got, want) {
		t.Errorf("INFRA-1's roles are %v, want %v: the reviewer's claim is gone, and INFRA-2's tester is another issue's", got, want)
	}
	var gotWait, wantWait map[string]any
	if err := json.Unmarshal(first.State.WaitingOn, &gotWait); err != nil {
		t.Fatalf("decode INFRA-1's wait %s: %v", first.State.WaitingOn, err)
	}
	if err := json.Unmarshal([]byte(wait), &wantWait); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotWait, wantWait) {
		t.Errorf("INFRA-1's wait decodes to %v, want the stored object %v", gotWait, wantWait)
	}
	if place, length, ok := first.Admission.Place("INFRA-1"); place != 2 || length != 2 || !ok {
		t.Errorf("INFRA-1 stands %d of %d (%t) in the line, want 2 of 2", place, length, ok)
	}
	if got, want := first.Admission.BlockersOf("INFRA-1"), []string{"INFRA-9", "INFRA-7"}; !slices.Equal(got, want) {
		t.Errorf("INFRA-1's blockers are %v, want %v", got, want)
	}

	for _, row := range []struct {
		what  string
		key   string
		phase string
		since time.Time
		seen  bool
	}{
		{"a phase that moved", "INFRA-2", PhaseImplementing, hour(2), true},
		{"a phase that came back after a null", "INFRA-3", PhasePlanning, hour(3), true},
		{"a phase written twice with one value", "INFRA-4", PhasePlanning, hour(2), false},
	} {
		got := read("INFRA", row.key)
		if !got.Recorded || got.State.Phase != row.phase || !got.State.PhaseSince.Equal(row.since) || got.PhaseStartSeen != row.seen {
			t.Errorf("%s, %s: recorded %t in %q since %s, seen to start %t; want recorded in %q since %s, seen to start %t",
				row.what, row.key, got.Recorded, got.State.Phase, got.State.PhaseSince, got.PhaseStartSeen, row.phase, row.since, row.seen)
		}
	}

	for _, key := range []string{"INFRA-5", "INFRA-6"} {
		got := read("INFRA", key)
		if got.Recorded || got.State.Phase != "" || got.PhaseStartSeen {
			t.Errorf("%s is recorded %t in %q, seen to start %t; want not recorded: its newest phase is null, or it has none",
				key, got.Recorded, got.State.Phase, got.PhaseStartSeen)
		}
		if got.Admission == nil {
			t.Errorf("%s's read carries no admission block, want its project's", key)
		}
	}

	other := read("OTHER", "OTHER-1")
	if !other.Recorded || other.Admission != nil {
		t.Errorf("OTHER-1 is recorded %t with the admission block %+v, want recorded with none: its project's newest block is null",
			other.Recorded, other.Admission)
	}
}
