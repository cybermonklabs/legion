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

// A root architect's row says whether the sweep's send of its entry again sends the entry's task
// as written (PhaseRow.EntryVerbatim), which only the resume of an answered root sets. The record
// holds that fact as it holds the task: written with the row and read back with it, both ways,
// and cleared with the task by a re-admission, whose promotion raises a new entry with work of
// its own. Both clears a re-admission makes are read: of one issue (ClearGeneration) and of a
// whole tree (ClearTreeGeneration). The entry and the moment it was raised stay, as they do for
// every role.
func TestTheArchitectEntryMarkToSendAsWrittenRoundTripsAndGoesWithTheGeneration(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	raised := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	const resumed = "A person answered your ask ask-1; read it. Continue."
	architectRow := func(key string) PhaseRow {
		t.Helper()
		var row PhaseRow
		inTx(t, st, func(tx pgx.Tx) {
			rows, err := records.Phases(ctx, tx, key)
			must(t, err)
			row = PhaseRowOf(rows, claim.RoleArchitect)
		})
		return row
	}
	for _, tc := range []struct {
		name  string
		key   string
		clear func(tx pgx.Tx, issue Issue) error
	}{
		{name: "a re-admission of the issue", key: "LEGION-291", clear: func(tx pgx.Tx, issue Issue) error {
			return records.ClearGeneration(ctx, tx, issue.Key, time.Now().UTC())
		}},
		{name: "a re-admission of the tree", key: "LEGION-292", clear: func(tx pgx.Tx, issue Issue) error {
			return records.ClearTreeGeneration(ctx, tx, issue.Tree, time.Now().UTC())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := issueFixture(tc.key)
			row := PhaseRow{Issue: issue.Key, Role: claim.RoleArchitect, Claim: "architect", Entry: 3,
				EntryTask: resumed, EntryVerbatim: true, EntryRaisedAt: raised}
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.PutIssue(ctx, tx, issue))
				must(t, records.PutPhase(ctx, tx, row))
			})
			if got := architectRow(issue.Key); got.EntryTask != resumed || !got.EntryVerbatim {
				t.Fatalf("the architect's row = task %q, sent as written %t; want %q marked to be sent as written, as it was put",
					got.EntryTask, got.EntryVerbatim, resumed)
			}
			row.EntryVerbatim = false
			inTx(t, st, func(tx pgx.Tx) { must(t, records.PutPhase(ctx, tx, row)) })
			if got := architectRow(issue.Key); got.EntryVerbatim {
				t.Fatal("the architect's row is still marked to be sent as written after a put that clears it, want the mark the put wrote")
			}
			row.EntryVerbatim = true
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.PutPhase(ctx, tx, row))
				must(t, tc.clear(tx, issue))
			})
			got := architectRow(issue.Key)
			if got.EntryTask != "" || got.EntryVerbatim {
				t.Fatalf("the architect's row after %s = task %q, sent as written %t; want no task and no mark: both were the generation's that ended",
					tc.name, got.EntryTask, got.EntryVerbatim)
			}
			if got.Entry != 3 || !got.EntryRaisedAt.Equal(raised) {
				t.Fatalf("the architect's row after %s = entry %d raised at %s, want the entry 3 raised at %s it stood on",
					tc.name, got.Entry, got.EntryRaisedAt, raised)
			}
		})
	}
}
