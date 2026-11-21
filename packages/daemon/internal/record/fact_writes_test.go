package record

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The end rule of a step's run reads two inputs the store writes - the issue's record and its pull
// request - and the fact's settle reads the runs of each issue its transaction wrote one of them
// for (IssuesWritten). So each writer of those inputs keeps its
// issue, once, in the order of its first write, in a ctx that keeps writes (WithIssueWrites); a
// writer that kept none would leave the run its write ends unended. A ctx that keeps no writes
// keeps nothing, which is the control.
func TestEachWriterOfAnEndInputKeepsItsIssue(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	keys := []string{"LEGION-301", "LEGION-302"}
	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range keys {
			must(t, store.PutIssue(t.Context(), tx, issueFixture(key)))
		}
	})
	write := func(ctx context.Context, tx pgx.Tx) {
		t.Helper()
		must(t, store.PutIssue(ctx, tx, issueFixture(keys[0])))
		must(t, store.PutPullRequest(ctx, tx, PullRequest{Issue: keys[1], Repo: "sjawhar/legion", Number: 1302, Branch: "legion/" + keys[1],
			HeadSHA: "b1", State: PullRequestOpen, Failing: []string{}, Cancelled: []string{}}))
		must(t, store.PutIssue(ctx, tx, issueFixture(keys[0])))
	}
	inTx(t, st, func(tx pgx.Tx) {
		kept := WithIssueWrites(t.Context())
		write(kept, tx)
		if got := IssuesWritten(kept); !slices.Equal(got, keys) {
			t.Fatalf("the issues the writes kept = %v, want each writer's issue once, in order: %v", got, keys)
		}
		plain := t.Context()
		write(plain, tx)
		if got := IssuesWritten(plain); got != nil {
			t.Fatalf("a ctx that keeps no writes kept %v, want nothing", got)
		}
	})
}
