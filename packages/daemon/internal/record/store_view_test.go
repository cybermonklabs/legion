package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A copy of an issue read for one of its tasks (Issue.ViewTask) stands in the phase of that task,
// which is not the issue's, so the store refuses to write it: the workflow's putIssue writes it, as
// the issue, in the phase each of its tasks derives. The refusal names the task and writes nothing,
// and the same issue read for no task is written as before.
//
// The fault it catches is a write of a copy read for a task that skips the derivation, which
// would leave the issue in one task's phase (its control: PutIssue without the refusal, which
// writes the copy's phase over the issue's).
func TestAWriteOfACopyReadForATaskIsRefused(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	issue := issueFixture("LEGION-208")
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutIssue(ctx, tx, issue)) })

	view := issue
	view.ViewTask, view.Phase = "a", phase.Reviewing
	err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error { return records.PutIssue(ctx, tx, view) })
	want := "put issue LEGION-208: a copy read for task a is written by the workflow's putIssue, which derives the issue's phase"
	if err == nil || err.Error() != want {
		t.Fatalf("the write of %s read for task a = %v, want the refusal %q", issue.Key, err, want)
	}
	inTx(t, st, func(tx pgx.Tx) {
		got, err := records.Issue(ctx, tx, issue.Key)
		must(t, err)
		if got == nil || got.Phase != phase.Implementing || got.ViewTask != "" {
			t.Fatalf("%s after the refused write = %+v, want it in implementing as it was written, read for no task", issue.Key, got)
		}
	})

	issue.Phase = phase.Testing
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutIssue(ctx, tx, issue)) })
	inTx(t, st, func(tx pgx.Tx) {
		got, err := records.Issue(ctx, tx, issue.Key)
		must(t, err)
		if got == nil || got.Phase != phase.Testing {
			t.Fatalf("%s written read for no task = %+v, want it in testing", issue.Key, got)
		}
	})
}
