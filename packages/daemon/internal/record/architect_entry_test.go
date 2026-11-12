package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A root's architect holds a row of the phases record carrying the entry its work stands at,
// that entry's task and the moment it was raised (PhaseRow.Entry, EntryTask, EntryRaisedAt), and
// its claim carries the entry it was queued a task of and the entry its worker took
// (supervise.Claim.GivenEntry and ServedEntry) — all of it exactly as a phase worker's row and
// claim do, read back through the same statements. So what the architect was handed, what that
// work is and how long it has stood are facts of the record, and a reader asks the architect's
// standing the same question it asks a tester's: is the entry the row names above the entry the
// claim was given?
func TestARootArchitectEntryIsHeldOnItsPhasesRowAsAPhaseWorkersIs(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-288")
	raised := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	const architectToken, testerToken = claim.Token("legion-legion-288-architect"), claim.Token("legion-legion-288-tester")
	const architectTask = "A person answered your ask ask-1 on LEGION-288."
	const testerTask = "Continue LEGION-288. Phase: testing."

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: issue.Key, Role: claim.RoleArchitect, Claim: architectToken,
			Entry: 3, EntryTask: architectTask, EntryRaisedAt: raised}))
		must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: testerToken,
			Entry: 2, EntryTask: testerTask, EntryRaisedAt: raised}))
	})
	// The architect was queued the work of entry 2 and its turn took it; the tester holds the
	// work of the entry its own row names. Both claims are read the same way.
	must(t, st.PutClaim(ctx, supervise.Claim{Token: architectToken, Project: issue.Project, Tree: issue.Tree,
		Issue: issue.Key, Role: claim.RoleArchitect, Generation: issue.Generation, State: supervise.StateReady,
		GivenEntry: 2, ServedEntry: 2}))
	must(t, st.PutClaim(ctx, supervise.Claim{Token: testerToken, Project: issue.Project, Tree: issue.Tree,
		Issue: issue.Key, Role: claim.RoleTester, Generation: issue.Generation, State: supervise.StateWorking,
		GivenEntry: 2, ServedEntry: 2}))

	rows := map[claim.Role]PhaseRow{}
	inTx(t, st, func(tx pgx.Tx) {
		read, err := records.Phases(ctx, tx, issue.Key)
		must(t, err)
		for _, row := range read {
			rows[row.Role] = row
		}
	})
	architect, held := rows[claim.RoleArchitect]
	if !held {
		t.Fatalf("%s holds no phases row of the architect, want the one the fixture wrote", issue.Key)
	}
	if architect.Entry != 3 || architect.EntryTask != architectTask || !architect.EntryRaisedAt.Equal(raised) {
		t.Fatalf("the architect's row = entry %d, task %q, raised at %s; want entry 3 carrying %q, raised at %s",
			architect.Entry, architect.EntryTask, architect.EntryRaisedAt, architectTask, raised)
	}

	// The two roles are read through one statement, and the architect's run answers as the
	// tester's does: its claim, and the entry that claim was queued a task of.
	assertOwed := func(what string, wantArchitect, wantTester bool) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			runs, err := records.RoleRuns(ctx, tx, []RoleRunKey{
				{Token: architectToken, Issue: issue.Key, Role: claim.RoleArchitect, Generation: issue.Generation},
				{Token: testerToken, Issue: issue.Key, Role: claim.RoleTester, Generation: issue.Generation}})
			must(t, err)
			for _, role := range []struct {
				role  claim.Role
				token claim.Token
				want  bool
			}{
				{claim.RoleArchitect, architectToken, wantArchitect},
				{claim.RoleTester, testerToken, wantTester},
			} {
				run := runs[role.token]
				if run.Claim == nil {
					t.Fatalf("the run of the %s carries no claim %s, want the one the fixture wrote", role.role, what)
				}
				row := rows[role.role]
				if owed := row.Entry > run.Claim.GivenEntry; owed != role.want {
					t.Fatalf("%s the %s is owed = %t on entry %d against the entry %d its claim was given, want owed = %t",
						what, role.role, owed, row.Entry, run.Claim.GivenEntry, role.want)
				}
			}
		})
	}
	assertOwed("as the fixture wrote them", true, false)

	// The entry the architect's claim is queued is what closes that difference, as it is for a
	// phase worker: queue it the work of the entry its row names and it holds its work.
	must(t, st.PutClaim(ctx, supervise.Claim{Token: architectToken, Project: issue.Project, Tree: issue.Tree,
		Issue: issue.Key, Role: claim.RoleArchitect, Generation: issue.Generation, State: supervise.StateWorking,
		GivenEntry: 3, ServedEntry: 2}))
	assertOwed("once the architect's claim was queued entry 3's task", false, false)

	// The entry its worker took is the claim's own record of the turn that confirmed that task,
	// and it is read back for the architect as for the tester.
	claims, err := st.Claims(ctx)
	must(t, err)
	served := map[claim.Token]supervise.Claim{}
	for _, held := range claims {
		served[held.Token] = held
	}
	if got := served[architectToken]; got.GivenEntry != 3 || got.ServedEntry != 2 {
		t.Fatalf("the architect's claim was given entry %d and served entry %d, want 3 and 2: it was queued entry 3's task and its turn took entry 2's",
			got.GivenEntry, got.ServedEntry)
	}
	if got := served[testerToken]; got.GivenEntry != 2 || got.ServedEntry != 2 {
		t.Fatalf("the tester's claim was given entry %d and served entry %d, want both 2", got.GivenEntry, got.ServedEntry)
	}
}
