package record

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The daemon counts its own status write for its own decisions before Dispatch echoes it (R16 of
// dispatch://INFRA-3320; the record's Status holds the write, its DispatchStatus what Dispatch
// last showed): a record whose Status holds the daemon's own backlog while its DispatchStatus
// still reads in_progress is out of Legion's work, and one whose Status holds the daemon's own
// in_progress while Dispatch still shows backlog is in it. A record at phase done is out of work
// whatever its status, and so is a status outside the five R15 names, one Dispatch adds later
// included.
//
// The chain reads the record and not the copy, which still shows Dispatch's status: an issue under
// that parent has it above in Legion's work exactly when the record says so.
func TestTheDaemonsOwnStatusCountsBeforeItsEcho(t *testing.T) {
	record := func(at phase.Phase, status, dispatchStatus string) Issue {
		issue := issueFixture("LEGION-2")
		issue.Parent, issue.Phase, issue.Status, issue.DispatchStatus = ParentOf("LEGION-1"), at, status, dispatchStatus
		return issue
	}
	for _, tc := range []struct {
		name   string
		issue  Issue
		inWork bool
	}{
		{"the daemon's own backlog before its echo", record(phase.Implementing, "backlog", "in_progress"), false},
		{"the daemon's own in_progress before its echo", record(phase.Implementing, "in_progress", "backlog"), true},
		{"todo", record(phase.Admitted, "todo", "todo"), true},
		{"testing", record(phase.Testing, "testing", "testing"), true},
		{"needs_review", record(phase.Reviewing, "needs_review", "needs_review"), true},
		{"retro", record(phase.Retro, "retro", "retro"), true},
		{"triage", record(phase.Implementing, "triage", "triage"), false},
		{"icebox", record(phase.Implementing, "icebox", "icebox"), false},
		{"done", record(phase.Merging, "done", "done"), false},
		{"phase done in a status in work", record(phase.Done, "in_progress", "in_progress"), false},
		{"a status Dispatch adds later", record(phase.Implementing, "blocked", "blocked"), false},
	} {
		if got := InLegionsWork(tc.issue); got != tc.inWork {
			t.Errorf("%s: a record at %s with status %s, Dispatch showing %s, in Legion's work = %t, want %t",
				tc.name, tc.issue.Phase, tc.issue.Status, tc.issue.DispatchStatus, got, tc.inWork)
		}
	}

	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	// LEGION-1 is a root at work, LEGION-2 its child and LEGION-3 at work under LEGION-2; the copy
	// shows LEGION-2 at the status Dispatch last showed, a sequence newer each time.
	for _, tc := range []struct {
		status, dispatchStatus string
		seq                    int64
		above                  []string
	}{
		{"backlog", "in_progress", 1, []string{"LEGION-1"}},
		{"in_progress", "backlog", 2, []string{"LEGION-2", "LEGION-1"}},
	} {
		inTx(t, st, func(tx pgx.Tx) {
			for _, written := range []struct {
				issue  Issue
				parent string
			}{
				{chainIssue("LEGION-1", "LEGION-1", "", phase.Implementing, "in_progress"), ""},
				{record(phase.Implementing, tc.status, tc.dispatchStatus), "LEGION-1"},
				{chainIssue("LEGION-3", "LEGION-1", "LEGION-2", phase.Implementing, "in_progress"), "LEGION-2"},
			} {
				must(t, records.PutIssue(ctx, tx, written.issue))
				must(t, records.PutDispatchIssue(ctx, tx, DispatchReading{Key: written.issue.Key, Project: "LEGION", Seq: tc.seq,
					Status: Carry(written.issue.DispatchStatus), Parent: Carry(written.parent), Blockers: Carry[[]string](nil)}))
			}
			chains, err := ReadChains(ctx, tx, records, "LEGION", nil)
			must(t, err)
			if got := chains.Above("LEGION-3"); !slices.Equal(got, tc.above) {
				t.Errorf("with LEGION-2 recorded %s and Dispatch still showing %s, the issues above LEGION-3 in Legion's work = %v, want %v",
					tc.status, tc.dispatchStatus, got, tc.above)
			}
		})
	}
}

// A project keeps the record of every issue it ever finished, so what a fact reads must be the
// records it can act on: every record of the project not in phase done, a done root a person set
// back to todo (re-admission reads it from the line), and a done record still holding a slot
// (the next release of slots frees it, and the cap counts it until then). A finished record, a
// parked child and another project's record are read only by key.
func TestLiveIssuesReadsTheRecordsOfTheProjectThatAreNotFinished(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	issue := func(key, project string, at phase.Phase, status string) Issue {
		recorded := issueFixture(key)
		recorded.Project, recorded.Phase, recorded.Status, recorded.Parent = project, at, status, nil
		return recorded
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, recorded := range []Issue{
			issue("LEGION-1", "LEGION", phase.Implementing, "in_progress"),
			issue("LEGION-2", "LEGION", phase.Done, "done"),
			issue("LEGION-3", "LEGION", phase.Done, "todo"),
			issue("LEGION-4", "LEGION", phase.Done, "done"),
			issue("LEGION-5", "LEGION", phase.Done, "backlog"),
			issue("LEGION-6", "LEGION", phase.Admitted, "todo"),
			issue("OTHER-1", "OTHER", phase.Implementing, "in_progress"),
		} {
			must(t, records.PutIssue(t.Context(), tx, recorded))
		}
		must(t, records.PutSlot(t.Context(), tx, Slot{Issue: "LEGION-4", Index: 0, AdmittedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)}))

		live, err := records.LiveIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("read the live issues: %v", err)
		}
		if got, want := keysOf(live), []string{"LEGION-1", "LEGION-3", "LEGION-4", "LEGION-6"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("live issues = %v, want %v: unfinished, set back to todo or holding a slot, by key", got, want)
		}

		named, err := records.IssuesOf(t.Context(), tx, "LEGION", []string{"LEGION-5", "OTHER-1", "LEGION-2", "LEGION-9"})
		if err != nil {
			t.Fatalf("read issues by key: %v", err)
		}
		if got, want := keysOf(named), []string{"LEGION-2", "LEGION-5"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("issues by key = %v, want %v: the project's recorded keys, by key", got, want)
		}
		if none, err := records.IssuesOf(t.Context(), tx, "LEGION", nil); err != nil || len(none) != 0 {
			t.Fatalf("issues of no key = %v, %v; want none", none, err)
		}
	})
}

// A judgement of an issue's blockers reads that issue's mirrored row and the rows of what it waits
// on, never the whole project's mirror, which keeps a row of every issue the project ever had. A
// blocker's own blockers are not read. A blocker of another project is, and under the project its
// own key names: an issue may wait on an issue outside the project, and a blocker whose row the
// judgement cannot read would count as unfinished for ever.
func TestDispatchIssuesOfReadsTheKeysAndWhatTheyWaitOn(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		for _, reading := range []DispatchReading{
			{Key: "LEGION-10", Project: "LEGION", Seq: 3, Status: Carry("todo"), Parent: Carry(""), Blockers: Carry([]string{"LEGION-12", "LEGION-11", "OTHER-11"})},
			{Key: "LEGION-11", Project: "LEGION", Seq: 4, Status: Carry("done"), Parent: Carry(""), Blockers: Carry[[]string](nil)},
			{Key: "LEGION-12", Project: "LEGION", Seq: 5, Status: Carry("todo"), Parent: Carry(""), Blockers: Carry([]string{"LEGION-13"})},
			{Key: "LEGION-13", Project: "LEGION", Seq: 6, Status: Carry("todo"), Parent: Carry(""), Blockers: Carry[[]string](nil)},
			{Key: "LEGION-14", Project: "LEGION", Seq: 7, Status: Carry("todo"), Parent: Carry(""), Blockers: Carry[[]string](nil)},
			{Key: "OTHER-11", Project: "OTHER", Seq: 8, Status: Carry("done"), Parent: Carry(""), Blockers: Carry[[]string](nil)},
		} {
			must(t, records.PutDispatchIssue(t.Context(), tx, reading))
		}
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-10", "session:s-1", 9))

		rows, err := records.DispatchIssuesOf(t.Context(), tx, []string{"LEGION-10"})
		if err != nil {
			t.Fatalf("read the mirror of LEGION-10: %v", err)
		}
		want := []DispatchIssueRow{
			{Key: "LEGION-10", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-12", "LEGION-11", "OTHER-11"}, Seq: 3, ClaimedBy: "session:s-1", ClaimSeq: 9, Labels: []string{}},
			{Key: "LEGION-11", Project: "LEGION", Status: "done", Blockers: []string{}, Seq: 4, Labels: []string{}},
			{Key: "LEGION-12", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-13"}, Seq: 5, Labels: []string{}},
			{Key: "OTHER-11", Project: "OTHER", Status: "done", Blockers: []string{}, Seq: 8, Labels: []string{}},
		}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("the mirror of LEGION-10 = %#v, want %#v: its row and its blockers', whatever project they are in, by key", rows, want)
		}
		if none, err := records.DispatchIssuesOf(t.Context(), tx, nil); err != nil || len(none) != 0 {
			t.Fatalf("the mirror of no key = %#v, %v; want none", none, err)
		}
	})
}

func keysOf(issues []Issue) []string {
	keys := make([]string, len(issues))
	for i, issue := range issues {
		keys[i] = issue.Key
	}
	return keys
}
