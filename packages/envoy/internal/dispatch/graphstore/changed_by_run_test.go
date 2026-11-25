package graphstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// changedByRunOf is every changed_by_run tuple out of the node to the run's interaction, oldest
// first.
func changedByRunOf(t *testing.T, database *store.Store, kind, id, run string) []Tuple {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select `+tupleColumns+` from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		order by seq
	`, kind, id, TupleChangedByRun, KindInteraction, run)
	if err != nil {
		t.Fatalf("read the changed_by_run of %s/%s to run %s: %v", kind, id, run, err)
	}
	tuples, err := scanTuples(rows)
	if err != nil {
		t.Fatalf("read the changed_by_run of %s/%s to run %s: %v", kind, id, run, err)
	}
	return tuples
}

// A write made for a run names each node its writer marks changed, once for each node and run:
// Caused appends the comment's changed_by_run under the event that stored it, dated and signed as
// the comment's own write, and its author's person node, which the write only names, gets none.
// A second write of the same comment for the same run - through Caused under a later event, and
// through Write - appends no second tuple; the same comment written for another run is named by
// that run too. A fact marked as changing its node (Tuple.Changes) names the node it goes out of
// and not its far end, and a write on a context holding no run names no node at all.
func TestAWriteMadeForARunNamesEachNodeItMarksOnce(t *testing.T) {
	database := storetest.Open(t)
	author := model.Actor{Kind: "user", ID: "run-writer@example.com"}
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	run, other := uuid.NewString(), uuid.NewString()
	comment, quiet := uuid.NewString(), uuid.NewString()
	const stored, edited = int64(9600001), int64(9600002)

	writeComment := func(ctx context.Context, tx pgx.Tx, id string, event int64) error {
		node, tuples := CommentFacts(nil, id, author, at, NoEvent, "", "", "", nil)
		node.Changed = true
		authors, authored, _ := AuthorFacts(KindComment, id, author, at, NoEvent)
		written := PlanUncaused(append([]Node{node}, authors...), append(tuples, authored...))
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			QueueNodes(b, written)
			return nil
		}); err != nil {
			return err
		}
		return Caused(ctx, tx, written, event)
	}
	forRun := store.WithRun(context.Background(), run)
	runInOneTransaction(t, database, "the comment written for the run", func(_ context.Context, tx pgx.Tx) error {
		return writeComment(forRun, tx, comment, stored)
	})
	named := changedByRunOf(t, database, KindComment, comment, run)
	t.Logf("the comment's changed_by_run once it is stored: %+v", named)
	if len(named) != 1 || !named[0].Asserted || named[0].SourceEvent != stored || !named[0].At.Equal(at) || !named[0].Who.SameCredential(author) {
		t.Fatalf("the comment written for run %s holds the changed_by_run %+v, want one, asserted under event %d, at %s by %+v",
			run, named, stored, at, author)
	}
	if held := changedByRunOf(t, database, KindPerson, author.ID, run); len(held) != 0 {
		t.Fatalf("the author's person node, which the write only names, holds the changed_by_run %+v", held)
	}

	runInOneTransaction(t, database, "the comment written again for the run", func(_ context.Context, tx pgx.Tx) error {
		return writeComment(forRun, tx, comment, edited)
	})
	marked, _ := CommentFacts(nil, comment, author, at, NoEvent, "", "", "", nil)
	marked.Changed = true
	runInOneTransaction(t, database, "the comment written through Write for the run", func(_ context.Context, tx pgx.Tx) error {
		return Write(forRun, tx, []Node{marked}, nil)
	})
	if again := changedByRunOf(t, database, KindComment, comment, run); len(again) != 1 || again[0].Seq != named[0].Seq {
		t.Fatalf("the comment written twice more for run %s holds the changed_by_run %+v, want the one it held, %+v", run, again, named)
	}

	runInOneTransaction(t, database, "the comment written for another run", func(_ context.Context, tx pgx.Tx) error {
		return Write(store.WithRun(context.Background(), other), tx, []Node{marked}, nil)
	})
	if held := changedByRunOf(t, database, KindComment, comment, other); len(held) != 1 || held[0].SourceEvent != NoEvent {
		t.Fatalf("the comment written for run %s holds the changed_by_run %+v to it, want one at the sentinel", other, held)
	}

	runInOneTransaction(t, database, "a comment written for no run", func(ctx context.Context, tx pgx.Tx) error {
		return writeComment(ctx, tx, quiet, stored+2)
	})
	for _, id := range []string{run, other} {
		if held := changedByRunOf(t, database, KindComment, quiet, id); len(held) != 0 {
			t.Fatalf("a comment written on a context with no run holds the changed_by_run %+v to run %s", held, id)
		}
	}

	pointer := Tuple{
		FromKind: KindComment, FromID: quiet, Kind: "mentions", ToKind: KindIssue, ToID: "RUN-1",
		At: at, Who: author, SourceEvent: NoEvent, Asserted: true, Changes: true,
	}
	runInOneTransaction(t, database, "a fact marked as changing its node, written for the run", func(_ context.Context, tx pgx.Tx) error {
		return Write(forRun, tx, nil, []Tuple{pointer, pointer})
	})
	if held := changedByRunOf(t, database, KindComment, quiet, run); len(held) != 1 {
		t.Fatalf("the node two marked facts go out of holds the changed_by_run %+v, want one", held)
	}
	if held := changedByRunOf(t, database, KindIssue, "RUN-1", run); len(held) != 0 {
		t.Fatalf("the far end of a marked fact holds the changed_by_run %+v, want none", held)
	}
}
