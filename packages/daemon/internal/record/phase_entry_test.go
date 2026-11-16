package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A re-admission clears the work a role's phase entry named and leaves the number where it
// stands. The task goes because it is the old generation's, cleared with that generation's
// handoffs (ClearGeneration): the tree runs again, and what the role is owed is whatever its
// promotion hands it, not the task of a run that ended. The number stays because it counts the
// entries the role has been handed on this issue and nothing of a re-admission unhands one: the
// promotion raises it again (workflow's StartPromotedWorker), so the entry the new work is sent
// under is above every entry a completion still in flight could name.
func TestClearGenerationClearsTheEntryTaskAndLeavesTheEntry(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-287")
	raised := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "implementer",
			Entry: 3, EntryTask: "Continue the change. Issue: LEGION-287. Phase: implementing.", EntryRaisedAt: raised}))
		must(t, records.ClearGeneration(ctx, tx, issue.Key, time.Now().UTC()))
	})
	inTx(t, st, func(tx pgx.Tx) {
		rows, err := records.Phases(ctx, tx, issue.Key)
		must(t, err)
		if len(rows) != 1 {
			t.Fatalf("phases after ClearGeneration = %#v, want the one row kept", rows)
		}
		row := rows[0]
		if row.EntryTask != "" {
			t.Fatalf("the implementer's row still names the work %q of the generation that ended, want none: its promotion writes the task it is given",
				row.EntryTask)
		}
		if row.Entry != 3 || !row.EntryRaisedAt.Equal(raised) {
			t.Fatalf("the implementer's row after ClearGeneration = entry %d raised at %s, want the entry 3 raised at %s it stood on",
				row.Entry, row.EntryRaisedAt, raised)
		}
	})
}
