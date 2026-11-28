package intake

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A view of an unrecorded issue reaches the handlers with the transition its preparation read
// (DispatchIssue.Transition), the writer of the status it shows, and with no writer of the event
// it was prepared in place of, at that event's own sequence or past it: the event's actor is no
// proof of who wrote what the view shows. A view whose preparation read no transition carries
// none.
func TestApplyFactKeepsAnUnrecordedViewsTransitionAndNoEventsWriter(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	var seen []DispatchIssue
	recorder := handlerFunc(func(_ context.Context, _ pgx.Tx, fact Fact) (Result, error) {
		seen = append(seen, fact.(DispatchIssue))
		return Result{}, nil
	})
	at := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	transition := &StatusTransition{Seq: 4, StatusBefore: "backlog", Session: "ses-implementer", At: at}
	event := DispatchIssue{Key: "LEGION-208", Seq: 5, Status: "todo", Parent: "LEGION-200", ActorSession: "ses-other", At: at.Add(time.Minute),
		ResolvedRepo: "acme/widgets"}
	for _, tc := range []struct {
		name       string
		seq        int64
		transition *StatusTransition
	}{
		{name: "at the event's own sequence", seq: 5, transition: transition},
		{name: "past the event's sequence", seq: 6, transition: transition},
		{name: "with no transition read", seq: 7},
	} {
		seen = nil
		view := DispatchIssue{Key: "LEGION-208", Seq: tc.seq, Status: "todo", Parent: "LEGION-200", ResolvedRepo: "acme/widgets",
			Observed: true, Delivered: &event, Transition: tc.transition}
		if _, err := ApplyFact(ctx, pool, "test", tc.name, view, recorder); err != nil {
			t.Fatalf("ApplyFact %s: %v", tc.name, err)
		}
		want := DispatchIssue{Key: "LEGION-208", Seq: tc.seq, Status: "todo", Parent: "LEGION-200", ResolvedRepo: "acme/widgets", Transition: tc.transition}
		if len(seen) != 1 || !reflect.DeepEqual(seen[0], want) {
			t.Fatalf("the handlers saw %+v for a view %s, want %+v", seen, tc.name, want)
		}
	}
}
