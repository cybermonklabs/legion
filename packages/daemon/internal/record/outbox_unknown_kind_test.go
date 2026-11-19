package record

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A row of a kind this binary holds in no lane is one a daemon of another release queued: a newer
// release's kind left in place across a rollback (docs/kubernetes.md, "Rolling back the release
// that records each step"), or a newer daemon's row during a rolling restart. This daemon has no
// executor for it, so the lanes it runs leave it out: an issue with only such a row has no lane,
// and an issue with such a row beside one of a kind it knows has that kind's lane alone. The
// outbox's check is dropped here to take the unknown kind, which this release's own check refuses.
func TestUnfinishedLanesPassOverAKindNoLaneHolds(t *testing.T) {
	st := migratedStore(t)
	const later = OutboxKind("a_later_releases_kind")
	if lane := LaneOf(later); lane != "" {
		t.Fatalf("LaneOf(%s) = %q, want no lane: the test needs a kind this binary does not know", later, lane)
	}
	lifecycle := LaneOf(OutboxKindSupervise)
	if lifecycle == "" {
		t.Fatalf("LaneOf(supervise) names no lane, so the known row below would prove nothing")
	}
	inTx(t, st, func(tx pgx.Tx) {
		if _, err := tx.Exec(t.Context(), `alter table outbox drop constraint outbox_kind_check`); err != nil {
			t.Fatalf("drop the kind check: %v", err)
		}
		for _, row := range []struct {
			issue string
			kind  OutboxKind
		}{{"LEGION-1", later}, {"LEGION-1", OutboxKindSupervise}, {"LEGION-2", later}} {
			if _, err := tx.Exec(t.Context(), `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
				values ($1, $2, '{"written": "by another release"}', 0, now(), '')`, string(row.kind), row.issue); err != nil {
				t.Fatalf("queue a %s row of %s: %v", row.kind, row.issue, err)
			}
		}
		lanes, err := NewStore().UnfinishedLanes(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("UnfinishedLanes: %v", err)
		}
		if want := []LaneKey{{Issue: "LEGION-1", Lane: lifecycle}}; !reflect.DeepEqual(lanes, want) {
			t.Fatalf("UnfinishedLanes = %+v, want %+v: a kind no lane holds starts no lane, and holds back no other", lanes, want)
		}
	})
}
