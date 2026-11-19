package graphcensus

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestTheReferenceBatchTakesTheStoreLockOrder holds writeReferenceBatch to the store's lock order,
// as TestEveryBackfillBatchTakesTheStoreLockOrder holds the other batches: the owner row the
// document stands under, its issue's row, first, then the mentions tuple. The tuple is stated at
// the sentinel, which no repeat key holds, so no hold stands on it, and the batch takes no project
// row. The document is one this build wrote with its node, beside which a process of the release
// before wrote a refs row to LOCK-2 that states no tuple. After the run the document's node states
// one mentions of LOCK-2, dated at the row's created_at and carrying its source_seq, and the
// backfill reports the row written.
func TestTheReferenceBatchTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedLockProject(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	createdAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	const sourceSeq int64 = 41
	var document, refKey string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('LOCK-1', 'LOCK', 1, 'Citing', '{"kind":"user","id":"alice"}', 'U'),
			       ('LOCK-2', 'LOCK', 2, 'Cited', '{"kind":"user","id":"alice"}', 'V')
		`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('LOCK-1', 'LOCK', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`).Scan(&document, &refKey); err != nil {
			return err
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.ArtifactNode(refKey, "LOCK", "", alice, createdAt)}, nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			update artifacts set node_kind = $2, node_address = ref_key where id = $1::uuid
		`, document, graphstore.KindArtifact); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into refs (from_kind, from_id, to_kind, to_id, created_at, source_seq)
			values ('artifact', $1, 'issue', 'LOCK-2', $2, $3)
		`, document, createdAt, sourceSeq)
		return err
	}); err != nil {
		t.Fatalf("seed the document and the reference the release before wrote: %v", err)
	}

	owner, project := holdLockOwner(t, database)
	report := followTheRun(t, database, "writeReferenceBatch", onePass(backfillReferences), []*lockstep.Hold{owner, project})
	held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindArtifact, refKey, []string{graphstore.TupleMentions})
	if err != nil {
		t.Fatalf("read the document's mentions: %v", err)
	}
	if len(held) != 1 || held[0].ToKind != graphstore.KindIssue || held[0].ToID != "LOCK-2" || !held[0].At.Equal(createdAt) ||
		held[0].LegacySourceSeq == nil || *held[0].LegacySourceSeq != sourceSeq || held[0].SourceEvent != graphstore.NoEvent {
		t.Errorf("the document states mentions %+v, want one to issue:LOCK-2 dated at the row's created_at %s, carrying its source_seq %d, at the sentinel: the run the test followed is not one that committed",
			held, createdAt, sourceSeq)
	}
	if done := reportedKind(t, report, referenceProjectionKind); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for the references, want 1 row, 1 tuple and none left", done)
	}
}
