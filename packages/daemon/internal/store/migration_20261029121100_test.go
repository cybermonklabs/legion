package store

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The turns a claim's producer reported are a jsonb window on the claim's own row, keyed by
// (generation, sequence). 20261029121100 adds the column with the empty array, which every row an older
// daemon wrote takes: no claim that ran before this release reported a turn, and the first report
// of each fills it.
func TestTheTurnOutcomesWindowIsAddedAt20261029121100(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 20261029121000)
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-LEGION-209-implementer', 'legion', 'LEGION-208', 'LEGION-209', 'implementer', 1, 'ses_implementer', '', 'idle', 0, 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if got := onlyClaim(t, store); len(got.TurnOutcomes) != 0 {
		t.Fatalf("a claim written before 20261029121100 reads back %+v, want no turn reported", got.TurnOutcomes)
	}
}

// The window round-trips as it is kept: in (generation, sequence) order, with each report's
// outcome and the provider's message, so a restarted daemon derives the same run of refused turns
// the one before it did.
func TestAClaimsTurnOutcomesRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if got := onlyClaim(t, store); len(got.TurnOutcomes) != 0 {
		t.Fatalf("a claim that reported no turn reads back %+v, want none", got.TurnOutcomes)
	}

	c.TurnOutcomes = []supervise.TurnOutcome{
		{Generation: 1, Sequence: 7, Refused: false},
		{Generation: 2, Sequence: 1, Refused: true, Message: "the provider refused the turn"},
	}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the claim with its reports: %v", err)
	}

	got := onlyClaim(t, store)
	sameClaim(t, got, c)
	if got.RefusedRun() != 1 || got.LastRefusal() != "the provider refused the turn" {
		t.Fatalf("the claim read back derives a run of %d ending %q, want 1 and the provider's message",
			got.RefusedRun(), got.LastRefusal())
	}
}

// A report whose write failed is kept in the window marked Unapplied, and the mark survives a
// restart: a new pool and a new store over the same database, with nothing of the first carried
// in memory (reopenStore). The mark is what tells the producer's repeat of that report, under the
// same key, from a report already applied (supervise's turnOutcome), so a daemon that read the
// window back without it would take the repeat for a report it had applied and leave undone what
// the report says of the claim's task.
func TestAnUnappliedTurnOutcomeRoundTripsMarked(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	c.TurnOutcomes = []supervise.TurnOutcome{
		{Generation: 1, Sequence: 6, Refused: false},
		{Generation: 1, Sequence: 7, Refused: true, Message: "the provider refused the turn", Unapplied: true},
	}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the claim with a report never applied: %v", err)
	}

	got := onlyClaim(t, reopenStore(t, store))
	if len(got.TurnOutcomes) != 2 || got.TurnOutcomes[0].Unapplied || !got.TurnOutcomes[1].Unapplied {
		t.Fatalf("the window read back after a restart = %+v, want the second report alone marked unapplied", got.TurnOutcomes)
	}
	sameClaim(t, got, c)
}

// A report's task number and the provider error it carried, with that error's status and the
// ladder step it made, are kept with the window and survive a restart, so a restarted daemon
// reads what an applied report did to the claim's pending task (supervise's owedReport). An entry
// an older daemon stored without them reads back with none of them, as a report that named no
// task and no provider error, which is how that daemon read it.
func TestTaskNumberAndProviderErrorOfAClaimsTurnOutcomesAreKept(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	c.TurnOutcomes = []supervise.TurnOutcome{
		{Generation: 1, Sequence: 6, Task: 4, ProviderError: true, ProviderErrorStatus: 529, ProviderErrorRetries: 2},
		{Generation: 1, Sequence: 7, Task: 4},
	}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the claim with its reports: %v", err)
	}
	reopened := reopenStore(t, store)
	got := onlyClaim(t, reopened)
	sameClaim(t, got, c)

	if _, err := reopened.Pool().Exec(ctx, `update claims set turn_outcomes = $2::jsonb where token = $1`, string(c.Token),
		`[{"generation": 1, "sequence": 7, "refused": true, "message": "the provider refused the turn"}]`); err != nil {
		t.Fatalf("store a window an older daemon wrote: %v", err)
	}
	want := []supervise.TurnOutcome{{Generation: 1, Sequence: 7, Refused: true, Message: "the provider refused the turn"}}
	if got := onlyClaim(t, reopened).TurnOutcomes; !slices.Equal(got, want) {
		t.Fatalf("the window an older daemon wrote reads back %+v, want %+v", got, want)
	}
}

// The window's watermark, the newest key the window dropped to keep to its bound, carried on its
// oldest entry, is kept with the window and survives a restart, so a restarted daemon still
// answers a report sent again after newer reports pushed it out of the window as the repeat it
// is (supervise's recordTurnOutcome). It needs no column of its own: it is a field of an entry
// in the window's own array, which a window an older daemon stored reads back without (above).
func TestTheTurnOutcomeWindowsWatermarkIsKept(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	mark := supervise.TurnOutcomeKey{Generation: 2, Sequence: 4}
	c.TurnOutcomes = []supervise.TurnOutcome{
		{Generation: 2, Sequence: 5, Watermark: mark},
		{Generation: 2, Sequence: 6},
		{Generation: 2, Sequence: 7, Task: 3},
	}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the claim with its window and its watermark: %v", err)
	}

	got := onlyClaim(t, reopenStore(t, store))
	sameClaim(t, got, c)
	if kept := got.TurnOutcomes; len(kept) != 3 || kept[0].Watermark != mark {
		t.Fatalf("the window read back after a restart = %+v, want its oldest entry carrying the watermark %+v", kept, mark)
	}
}
