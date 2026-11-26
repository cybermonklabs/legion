package graphcensus

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// seedAskWithoutBlockTuple writes an ask whose row and node the earlier release already marked.
func seedAskWithoutBlockTuple(t *testing.T, database *store.Store, project, issue, document, refKey string) (id string, createdAt time.Time) {
	t.Helper()
	ctx := t.Context()
	err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			with next as (select gen_random_uuid() as id)
			insert into asks (id, issue_key, author, question, block_artifact_id, block_id, node_kind, node_address)
			select id, $1, '{"kind":"user","id":"alice"}', 'Where does this ask stand?', $2::uuid, 'q1', 'ask', id::text
			from next returning id::text, created_at`, issue, document).Scan(&id, &createdAt); err != nil {
			return err
		}
		nodes, tuples, _ := graphstore.AskFacts(id, project, nil, model.Actor{Kind: "user", ID: "alice"}, createdAt,
			graphstore.NoEvent, graphstore.AskPlaces{Anchor: refKey, Document: refKey})
		return graphstore.Write(ctx, tx, nodes, tuples)
	})
	if err != nil {
		t.Fatalf("seed the earlier release's ask: %v", err)
	}
	return id, createdAt
}

func TestTheAskedOnBatchTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	seedLockProject(t, database)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('LOCK-1', 'LOCK', 1, 'Asked on a block', '{"kind":"user","id":"alice"}', 'U')`); err != nil {
		t.Fatal(err)
	}
	var document, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('LOCK-1', 'LOCK', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key`).Scan(&document, &refKey); err != nil {
		t.Fatal(err)
	}
	id, createdAt := seedAskWithoutBlockTuple(t, database, "LOCK", "LOCK-1", document, refKey)
	check := func(rows, tuples, missing, extra int64) {
		t.Helper()
		got := censusKind(t, storeCensusOf(t, database), graphstore.TupleAskedOn)
		if got.Rows != rows || got.Nodes != tuples || got.RowDifferences != missing || got.NodeDifferences != extra {
			t.Errorf("asked_on census: %+v; want %d rows, %d tuples, %d missing and %d extra", got, rows, tuples, missing, extra)
		}
	}
	check(1, 0, 1, 0)
	owner, project := holdLockOwner(t, database)
	report := followTheRun(t, database, "writeAskedOnBatch", onePass(backfillAskedOn), []*lockstep.Hold{owner, project})
	if done := reportedKind(t, report, graphstore.TupleAskedOn); !done.Complete || done.Rows != 1 || done.Tuples != 1 {
		t.Errorf("asked_on backfill: %+v; want one row and one tuple, complete", done)
	}
	held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindAsk, id, []string{graphstore.TupleAskedOn})
	if err != nil || len(held) != 1 || held[0].ToKind != refs.BlockNodeKind || held[0].ToID != refKey+"#q1" || !held[0].At.Equal(createdAt) {
		t.Fatalf("backfilled asked_on = %+v (%v), want the block %s#q1 at %s", held, err, refKey, createdAt)
	}
	check(1, 1, 0, 0)
	if done, err := backfillAskedOn(ctx, database); err != nil || !done.Complete || done.Rows != 0 || done.Tuples != 0 {
		t.Errorf("second backfill = %+v (%v), want no work", done, err)
	}
	appendTuple(t, database, graphstore.Tuple{
		FromKind: graphstore.KindAsk, FromID: id, Kind: graphstore.TupleAskedOn,
		ToKind: refs.BlockNodeKind, ToID: refKey + "#wrong", Asserted: true,
	})
	check(1, 2, 0, 1)
	if done, err := backfillAskedOn(ctx, database); err != nil || !done.Complete || done.Rows != 1 || done.Tuples != 1 {
		t.Errorf("backfill with an extra tuple = %+v (%v), want one retraction", done, err)
	}
	check(1, 1, 0, 0)
}
