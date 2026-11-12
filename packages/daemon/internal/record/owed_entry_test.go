package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Which roles are owed work is a question the record answers on its own, with every outbox row
// of the issue deleted: the role's phases row names the entry the transition raised and the work
// it named (PhaseRow.Entry, PhaseRow.EntryTask), and its claim names the entry it was last
// queued a task of (RoleClaim.GivenEntry, which RoleRuns reads beside the queued rows). A claim
// below its row's entry is owed that entry's work, whether the claim is live or suspended, and a
// role the daemon holds no claim row of is owed it with nothing to read but the row.
func TestTheRecordSaysWhichRolesAreOwedWorkWithEveryOutboxRowDeleted(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-288")
	raised := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	roles := []struct {
		role    claim.Role
		token   claim.Token
		task    string
		entry   int64
		given   int64
		claimed bool
		state   supervise.ClaimState
		by      supervise.SuspendSource
		owed    bool
	}{
		{
			role: claim.RoleImplementer, token: "legion-legion-288-implementer",
			task: "Implement it.", entry: 4, given: 4, claimed: true, state: supervise.StateWorking,
		},
		{
			role: claim.RoleTester, token: "legion-legion-288-tester",
			task: "Test it.", entry: 2, given: 1, claimed: true, state: supervise.StateReady, owed: true,
		},
		{
			role: claim.RoleReviewer, token: "legion-legion-288-reviewer",
			task: "Review it.", entry: 1, claimed: true, state: supervise.StateSuspended,
			by: supervise.SuspendedByDaemon, owed: true,
		},
		{role: claim.RoleMerger, token: "legion-legion-288-merger", task: "Merge it.", entry: 1, owed: true},
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		for _, r := range roles {
			must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: issue.Key, Role: r.role, Claim: r.token,
				Entry: r.entry, EntryTask: r.task, EntryRaisedAt: raised}))
		}
		// The tester's start is in the outbox before the delete below, so the read answers over
		// an issue whose rows were there and are gone.
		queued, err := NewOutboxRow(issue.Key, SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester,
			Generation: issue.Generation, Phase: phase.Testing, Entry: 2, Task: "Test it.", NeedsSlot: true}, time.Now().UTC())
		must(t, err)
		must(t, records.Enqueue(ctx, tx, queued))
	})
	for _, r := range roles {
		if !r.claimed {
			continue
		}
		must(t, st.PutClaim(ctx, supervise.Claim{Token: r.token, Project: issue.Project, Tree: issue.Tree,
			Issue: issue.Key, Role: r.role, Generation: issue.Generation, State: r.state, SuspendedBy: r.by,
			GivenEntry: r.given}))
	}
	inTx(t, st, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `delete from outbox where issue = $1`, issue.Key); err != nil {
			t.Fatalf("delete the issue's outbox rows: %v", err)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		rows, err := records.Phases(ctx, tx, issue.Key)
		must(t, err)
		entries := make(map[claim.Role]PhaseRow, len(rows))
		for _, row := range rows {
			entries[row.Role] = row
		}
		keys := make([]RoleRunKey, 0, len(roles))
		for _, r := range roles {
			keys = append(keys, RoleRunKey{Token: r.token, Issue: issue.Key, Role: r.role, Generation: issue.Generation})
		}
		runs, err := records.RoleRuns(ctx, tx, keys)
		must(t, err)

		for _, r := range roles {
			run := runs[r.token]
			if len(run.Queued) != 0 {
				t.Fatalf("the run of the %s carries %d queued rows, want none: every outbox row of %s is deleted",
					r.role, len(run.Queued), issue.Key)
			}
			switch {
			case !r.claimed && run.Claim != nil:
				t.Fatalf("the run of the %s carries claim %+v, want none: the daemon holds no claim row of it", r.role, run.Claim)
			case r.claimed && run.Claim == nil:
				t.Fatalf("the run of the %s carries no claim, want the one the fixture wrote", r.role)
			case r.claimed && run.Claim.State != r.state:
				t.Fatalf("the %s's claim reads %s, want %s", r.role, run.Claim.State, r.state)
			}
			row, recorded := entries[r.role]
			if !recorded {
				t.Fatalf("%s holds no phases row of the %s, want the one the fixture wrote", issue.Key, r.role)
			}
			given := int64(0)
			if run.Claim != nil {
				given = run.Claim.GivenEntry
			}
			if owed := row.Entry > given; owed != r.owed {
				t.Fatalf("the %s is owed = %t on entry %d against the entry %d its claim was given, want owed = %t",
					r.role, owed, row.Entry, given, r.owed)
			}
			if r.owed && row.EntryTask != r.task {
				t.Fatalf("the %s is owed entry %d and its row names the work %q, want %q: what it is owed is the record's",
					r.role, row.Entry, row.EntryTask, r.task)
			}
		}
	})
}
