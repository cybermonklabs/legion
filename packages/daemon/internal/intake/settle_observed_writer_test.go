package intake

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A view of an unrecorded issue that stands at the sequence of the event it was prepared in place
// of shows the state that event wrote, so settleObserved carries that event's writer — its session,
// its time and the status Dispatch showed before it — onto the plain observation the handlers see.
// A view Dispatch has run past carries none of them.
func TestApplyFactCarriesTheWriterOntoAnUnrecordedViewAtItsEventsSequence(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	var seen []DispatchIssue
	recorder := handlerFunc(func(_ context.Context, _ pgx.Tx, fact Fact) (Result, error) {
		seen = append(seen, fact.(DispatchIssue))
		return Result{}, nil
	})
	at := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	event := DispatchIssue{Key: "LEGION-208", Seq: 5, Status: "todo", Parent: "LEGION-200",
		ActorSession: "ses-implementer", At: at, StatusBefore: "backlog", ResolvedRepo: "acme/widgets"}
	sameSeq := DispatchIssue{Key: "LEGION-208", Seq: 5, Status: "todo", Parent: "LEGION-200",
		ResolvedRepo: "acme/widgets", Observed: true, Delivered: &event}
	if _, err := ApplyFact(ctx, pool, "test", "same-seq", sameSeq, recorder); err != nil {
		t.Fatalf("ApplyFact same-seq: %v", err)
	}
	wantSame := DispatchIssue{Key: "LEGION-208", Seq: 5, Status: "todo", Parent: "LEGION-200",
		ActorSession: "ses-implementer", At: at, StatusBefore: "backlog", ResolvedRepo: "acme/widgets"}
	if len(seen) != 1 || !reflect.DeepEqual(seen[0], wantSame) {
		t.Fatalf("the handlers saw %+v for a view at its event's own sequence, want %+v", seen, wantSame)
	}
	laterSeq := DispatchIssue{Key: "LEGION-208", Seq: 6, Status: "todo", Parent: "LEGION-200",
		ResolvedRepo: "acme/widgets", Observed: true, Delivered: &event}
	if _, err := ApplyFact(ctx, pool, "test", "later-seq", laterSeq, recorder); err != nil {
		t.Fatalf("ApplyFact later-seq: %v", err)
	}
	wantLater := DispatchIssue{Key: "LEGION-208", Seq: 6, Status: "todo", Parent: "LEGION-200", ResolvedRepo: "acme/widgets"}
	if len(seen) != 2 || !reflect.DeepEqual(seen[1], wantLater) {
		t.Fatalf("the handlers saw %+v for a view past its event's sequence, want %+v", seen[1], wantLater)
	}
}
