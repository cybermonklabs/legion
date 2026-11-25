package record

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A role of the deployment that stands at two steps of a task has a row for each, and the issue's
// roles name it once: each walk of them (a close, a linger's end, a workspace's removal) reaches
// its claim once. Before, the role was listed once for each of its rows.
func TestIssueRolesNameARoleAtTwoStepsOnce(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-296")
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		for _, place := range []int{2, 4} {
			must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: issue.Key, Role: "checker", Claim: "legion-legion-legion-296-checker",
				StepTask: "a", StepPlace: place, Entry: int64(place)}))
		}
		roles, err := IssueRoles(ctx, tx, records, issue.Key)
		must(t, err)
		if want := append(append([]claim.Role{}, claim.Roles...), "checker"); !reflect.DeepEqual(roles, want) {
			t.Fatalf("IssueRoles = %v, want %v: the checker once", roles, want)
		}
	})
}

// An issue in implementing whose implementer's row now holds no handoff is no issue handed off
// with no pull request, whatever an earlier step of the implementer reported: in `implementer:a
// reviewer implementer:a` at step 3, before its completion, step 1's row holds the commit it
// reported and step 3's row none. Once step 3 reports, the issue is listed once, with step 3's
// commit. Before, the read joined every row of the implementer, so it listed the issue on step 1's
// commit, and twice once both had reported.
func TestAnIssueHandedOffIsReadFromItsImplementersRowNow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-297")
	if issue.Phase != phase.Implementing {
		t.Fatalf("the fixture stands in %s, want implementing, where the read looks", issue.Phase)
	}
	at := func(minute int) time.Time { return time.Date(2026, 11, 22, 7, minute, 0, 0, time.UTC) }
	read := func(tx pgx.Tx) []HandedOff {
		t.Helper()
		handedOff, err := records.HandedOffWithoutOpenOrMergedPullRequest(ctx, tx, issue.Project)
		must(t, err)
		return handedOff
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "legion-legion-legion-297-implementer",
			StepTask: "a", StepPlace: 1, Entry: 1, EntryRaisedAt: at(1), HandoffCommit: "step-1-head"}))
		step3 := PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "legion-legion-legion-297-implementer",
			StepTask: "a", StepPlace: 3, Entry: 3, EntryRaisedAt: at(3)}
		must(t, records.PutPhase(ctx, tx, step3))
		if got := read(tx); len(got) != 0 {
			t.Fatalf("the issues handed off at step 3 before its report = %+v, want none: the implementer's row now holds no handoff", got)
		}
		step3.HandoffCommit = "step-3-head"
		must(t, records.PutPhase(ctx, tx, step3))
		if got := read(tx); len(got) != 1 || got[0].Issue.Key != issue.Key || got[0].Commit != "step-3-head" {
			t.Fatalf("the issues handed off after step 3's report = %+v, want %s once, at step 3's commit", got, issue.Key)
		}
	})
}
