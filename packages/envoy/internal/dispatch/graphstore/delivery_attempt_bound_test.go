package graphstore

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An attempt's number is the int4 its row holds, so a key whose attempt lies past that range names
// no row of either attempt table. The statement that states it is refused - pgx encodes the
// attempts it binds as the `int[]` it names and refuses one outside int4 - and the batch it is
// queued in fails with it, so a fact queued before it is never committed. Narrowed to 32 bits in
// Go, the number would wrap to a negative one, match no row, and let the batch commit the rest as
// if the attempt had been stated.
func TestAnAttemptPastInt4IsRefusedNotWrapped(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	for name, key := range map[string]AttemptKey{
		"a comment's attempt": CommentAttempt("00000000-0000-4000-8000-000000000001", "session:s1", math.MaxInt32+1),
		"a message's attempt": MessageAttempt("00000000-0000-4000-8000-000000000002", math.MaxInt32+1),
	} {
		t.Run(name, func(t *testing.T) {
			marker := Tuple{
				FromKind: KindSession, FromID: "attempt-bound " + name, Kind: "attempt_bound_marker",
				ToKind: KindValue, ToID: "queued before the attempt",
				At: time.Now().UTC(), Who: AttemptWriter, SourceEvent: NoEvent, Asserted: true,
			}
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			err = store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				QueueWrite(b, nil, []Tuple{marker})
				QueueAttempted(b, AttemptWriter, NoEvent, key)
				return nil
			})
			if err == nil {
				if err := tx.Commit(ctx); err != nil {
					t.Fatalf("commit the batch that took attempt %d: %v", key.Attempt, err)
				}
				t.Errorf("stating attempt %d of %s was taken, want it refused: no row holds a number past int4", key.Attempt, key.ID())
			} else {
				_ = tx.Rollback(ctx)
				t.Logf("stating attempt %d is refused: %v", key.Attempt, err)
			}
			var written int
			if err := database.Pool.QueryRow(ctx,
				`select count(*) from graph_tuples where from_kind = $1 and from_id = $2`, marker.FromKind, marker.FromID,
			).Scan(&written); err != nil {
				t.Fatalf("count the facts the batch wrote: %v", err)
			}
			if written != 0 {
				t.Errorf("the batch holding attempt %d committed %d fact(s) queued before it, want none", key.Attempt, written)
			}
		})
	}
}
