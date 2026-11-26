package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Task and agent are independent parts of the address: neither a second task of one agent nor
// a second agent of one task may replace a row. A generation clears only the live attachment.
func TestPhaseRowsKeepTheirTaskAndAgentAcrossAGeneration(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	rows := []PhaseRow{
		{Issue: planIssue, Role: claim.RoleReviewer, Claim: "legion-legion-legion-208-reviewer", StepTask: "a", StepPlace: 2, Entry: 7, EntryTask: "review a"},
		{Issue: planIssue, Role: claim.RoleReviewer, Claim: "legion-legion-legion-208-reviewer", StepTask: "b", StepPlace: 3, Entry: 8, EntryTask: "review b"},
		{Issue: planIssue, Role: claim.RoleReviewer, Claim: "legion-legion-legion-208-reviewer-2", StepTask: "a", StepPlace: 2, Entry: 9, EntryTask: "another review of a"},
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStore().PutIssue(ctx, tx, issueFixture(planIssue)))
		for _, row := range rows {
			must(t, NewStore().PutPhase(ctx, tx, row))
		}
		stored, err := NewStore().Phases(ctx, tx, planIssue)
		must(t, err)
		entries, err := NewStore().PhaseEntries(ctx, tx, []string{planIssue})
		must(t, err)
		if len(stored) != len(rows) || len(entries) != len(rows) {
			t.Fatalf("phase identities collapsed: %d stored rows and %d entry keys, want %d of each", len(stored), len(entries), len(rows))
		}
		for _, want := range rows {
			got := PhaseRowAt(stored, want.Key())
			if got.Entry != want.Entry || got.EntryTask != want.EntryTask || got.StepTask != want.StepTask || got.StepPlace != want.StepPlace {
				t.Fatalf("phase %v = %+v, want its own row %+v", want.Key(), got, want)
			}
			if entry := entries[want.Key()]; entry.Entry != want.Entry || entry.Claim != want.Claim || entry.StepTask != want.StepTask {
				t.Fatalf("batched phase %v = %+v, want entry %d of %s", want.Key(), entry, want.Entry, want.Claim)
			}
		}
		changed := PhaseRowAt(stored, rows[0].Key())
		changed.EntryTask = "changed a only"
		must(t, NewStore().PutPhase(ctx, tx, changed))
		stored, err = NewStore().Phases(ctx, tx, planIssue)
		must(t, err)
		for _, want := range rows[1:] {
			if got := PhaseRowAt(stored, want.Key()); got.EntryTask != want.EntryTask {
				t.Fatalf("updating %v replaced %v: %+v", rows[0].Key(), want.Key(), got)
			}
		}
		must(t, NewStore().ClearGeneration(ctx, tx, planIssue, planGivenAt))
		stored, err = NewStore().Phases(ctx, tx, planIssue)
		must(t, err)
		entries, err = NewStore().PhaseEntries(ctx, tx, []string{planIssue})
		must(t, err)
		if len(stored) != len(rows) || len(entries) != len(rows) {
			t.Fatalf("a generation merged phase identities: %d rows and %d entry keys, want %d", len(stored), len(entries), len(rows))
		}
		for _, want := range rows {
			got := PhaseRowAt(stored, want.Key())
			if got.Key() != want.Key() || got.Entry != want.Entry || got.StepTask != "" || got.StepPlace != 0 || got.EntryTask != "" {
				t.Fatalf("phase %v after generation reset = %+v; keep its identity and entry, clear its current attachment", want.Key(), got)
			}
			if entry, ok := entries[want.Key()]; !ok || entry.Entry != want.Entry || entry.StepTask != "" {
				t.Fatalf("batched phase %v after generation reset = %+v, present %t", want.Key(), entry, ok)
			}
		}
	})
}
