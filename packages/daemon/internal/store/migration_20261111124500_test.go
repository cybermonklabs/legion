package store

import (
	"context"
	"strings"
	"testing"
)

// mergeJumpOutcomeVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const mergeJumpOutcomeVersion = 20261111124500

// A jump record written under 20261110235352 says what became of its jump as the absence of a
// refusal, which is three states at once (record.MergeJumpOutcome). 20261111124500 replaces that
// pair of columns with the outcome and its answer, and what it does with a row that already
// exists is the whole reason it is a second migration rather than an edit of the first: every
// suite in this package builds from empty, so only a database that already carries
// 20261110235352 can exercise it.
//
// The mapping it is held to:
//
//   - refused = true becomes outcome 'refused', with its words carried into answer as they
//     stand, since that row recorded an answer that decided the jump.
//   - refused = false becomes outcome 'asked'. It is not 'accepted' and not 'unknown': the old
//     false meant no more than "no refusal recorded" and covered Legion asking and never
//     learning, GitHub accepting, and a worker dying before it wrote what happened, with nothing
//     in the row to tell them apart. 'asked' is the only one of the four such a row supports.
//
// The check constraint is read here too, by the value it has to refuse: a fifth word in the
// column would put a reader back to guessing what it meant.
func TestJumpRecordsPredating20261111124500MigrateToAskedAndRefused(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, mergeJumpOutcomeVersion-1)

	// Two rows of the old shape, as the daemon of 20261110235352 wrote them: one whose jump
	// GitHub refused with words, and one the daemon never wrote again after the mutation.
	const old = `insert into merge_jump_records (project, repository, issue, number, head, at, reason,
		entries, entries_total, entries_unknown, refused, refusal)
		values ($1, $2, $3, $4, $5, now(), 'the release waits on it', '[]', 0, true, $6, $7)`
	for _, row := range []struct {
		repository, issue, head string
		refused                 bool
		refusal                 string
	}{
		{repository: "acme/widgets", issue: "LEGION-208", head: "c0de", refused: true,
			refusal: "GitHub refused the jump: the merge queue of acme/widgets takes no jump"},
		{repository: "acme/other", issue: "LEGION-209", head: "beef"},
	} {
		if _, err := store.Pool().Exec(ctx, old, "LEGION", row.repository, row.issue, 42, row.head, row.refused, row.refusal); err != nil {
			t.Fatalf("write the old-shape record of %s: %v", row.repository, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the jump outcome: %v", err)
	}

	for _, want := range []struct {
		repository, outcome, answer string
	}{
		{repository: "acme/widgets", outcome: "refused",
			answer: "GitHub refused the jump: the merge queue of acme/widgets takes no jump"},
		{repository: "acme/other", outcome: "asked", answer: ""},
	} {
		var outcome, answer string
		if err := store.Pool().QueryRow(ctx, `select outcome, answer from merge_jump_records
			where project = 'LEGION' and repository = $1`, want.repository).Scan(&outcome, &answer); err != nil {
			t.Fatalf("read the migrated record of %s: %v", want.repository, err)
		}
		if outcome != want.outcome || answer != want.answer {
			t.Fatalf("the migrated record of %s reads %s with %q, want %s with %q: a row that recorded no refusal supports no outcome but asked",
				want.repository, outcome, answer, want.outcome, want.answer)
		}
	}

	// The columns the outcome replaces are gone, so nothing reads a jump's end out of them.
	for _, column := range []string{"refused", "refusal"} {
		var left int
		if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.columns
			where table_name = 'merge_jump_records' and column_name = $1`, column).Scan(&left); err != nil {
			t.Fatalf("read whether merge_jump_records still carries %s: %v", column, err)
		}
		if left != 0 {
			t.Fatalf("merge_jump_records still carries %s, want it dropped with the inference it stood for", column)
		}
	}

	// Only the four values the record names go in the column, and nothing goes in it by default:
	// the writer says what happened or writes nothing at all.
	for _, outcome := range []string{"asked", "accepted", "refused", "unknown"} {
		if _, err := store.Pool().Exec(ctx, `insert into merge_jump_records (project, repository, issue, number, head, at, reason,
			entries, entries_total, entries_unknown, outcome, answer)
			values ('LEGION', $1, 'LEGION-210', 43, 'cafe', now(), 'one of each', '[]', 0, false, $2, '')`,
			"acme/"+outcome, outcome); err != nil {
			t.Fatalf("write a record whose outcome is %s: %v", outcome, err)
		}
	}
	_, err := store.Pool().Exec(ctx, `insert into merge_jump_records (project, repository, issue, number, head, at, reason,
		entries, entries_total, entries_unknown, outcome, answer)
		values ('LEGION', 'acme/fifth', 'LEGION-211', 44, 'f00d', now(), 'a fifth word', '[]', 0, false, 'queued', '')`)
	if err == nil || !strings.Contains(err.Error(), "merge_jump_records_outcome_check") {
		t.Fatalf("a record whose outcome is 'queued' = %v, want the check constraint to refuse it", err)
	}
}
