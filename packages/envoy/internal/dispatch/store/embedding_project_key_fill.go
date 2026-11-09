package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// EmbeddingProjectKeyBatch is how many embeddings rows one transaction of
// FillEmbeddingProjectKeys labels. Each row carries a 1,536-float vector, so writing its
// project_key rewrites about 6 KB: the load-test deployment labelled 120,000 rows in 12 m 50 s
// and 19,650 in 86 s, which is 156 to 228 rows a second (INFRA-988,
// 20261109120100_embeddings_project_key_backfill.up.sql). 200 rows is about a second of that,
// which is the longest a live write of a row this batch has already taken - the enqueue of a
// window whose text just changed - waits for the batch's commit. Smaller batches would buy
// that writer little and cost one more commit, one more `for update` of the cursor and one
// more descent of embeddings_pkey each.
const EmbeddingProjectKeyBatch = 200

// embeddingProjectKeyProgressEvery is how often a running fill says where it has got to. The pass
// is about an hour on a deployment of a million vectors and labels 156 to 228 rows a second
// (EmbeddingProjectKeyBatch's measurements), so a line every half minute is about 120 lines for
// the whole of it: enough for a reader of a rolling deploy to see the row count climbing within
// one scroll of the log, and far short of the one line per batch - 5,000 of them - that logging
// each second of work would be.
//
// It is a variable only so a test can see a progress line without labelling a deployment's worth
// of rows, as duty's jitterN is a variable only so a test can fix the draw
// (internal/dispatch/duty/ticker.go:22).
var embeddingProjectKeyProgressEvery = 30 * time.Second

// fillEmbeddingProjectKeysCursorQuery reads the cursor the next batch starts from. It is a
// statement of its own so that the batch below can be planned against the values it answers,
// bound as parameters, which is what lets the planner read them as the constants they are.
//
// The cursor is read `for update`, which is what makes two processes filling at once take
// different batches rather than the same one: the lock is held to the batch's commit, so the
// second waits for the first and then reads the cursor it moved. The pass also runs under the
// lease embeddings-project-key-fill (api.RunEmbeddingProjectKeyFill), which keeps a second
// process out of the fill altogether; this lock is what still holds where a lease has expired
// under a process that stalled rather than stopped, and it costs one read of a one-row table
// per batch.
//
// Why the cursor is not a CTE of the batch any longer. It was `with progress as (select kind,
// id from embeddings_project_key_backfill for update)`, joined to embeddings in the batch's
// own CTE, and the planner made that join a nested loop whose inner side is the cursor:
// measured on the load-test deployment's own database, 1,104,667 embedded rows,
// `EXPLAIN (ANALYZE, BUFFERS)` of the batch as the fill issues it read
//
//	->  CTE Scan on progress p (actual time=316.918..316.918 rows=1.00 loops=400)
//	->  Index Scan using embeddings_pkey on embeddings e (actual time=0.028..1.794 rows=400.00 loops=1)
//	Rows Removed by Join Filter: 200
//	Execution Time: 126769.773 ms
//
// - 127 s for one batch of 200 rows, which is about eight days for the table, and the whole of
// it is the cursor re-read once per candidate row. A cursor the planner cannot see is also a
// cursor it cannot push into the index: the scan answered 400 rows for a batch of 200 and a
// join filter threw half of them away, where a bound pair is an index condition and the scan
// answers exactly the batch. The shape passed its tests and a warm reading of it because on a
// small warm database the same statement is fast - the fill labelled 2,200 rows in 30 s there -
// so the test that holds this shape in place is a plan test, not a timing one
// (TestTheEmbeddingProjectKeyBatchReadsOneBoundedIndexScan).
const fillEmbeddingProjectKeysCursorQuery = `select kind, id from embeddings_project_key_backfill for update`

// fillEmbeddingProjectKeysBatchQuery is one batch: it takes the next EmbeddingProjectKeyBatch
// rows by keyset from the cursor, labels each one whose project_key is still null from the
// owner the row belongs to, moves the cursor to the last row it took, and answers the rows it
// labelled and whether the pass is finished. $1 and $2 are the cursor
// fillEmbeddingProjectKeysCursorQuery answered, $3 the batch size.
//
// `batch` is the keyset: the next rows above the cursor in (kind, id) order, which is
// embeddings' own primary key, so the scan starts where the last batch stopped instead of
// counting past it as an `offset` would. It takes the rows of every kind, the issue's whole-text
// rows among the four window kinds, because every arm of meaning search now reads the column
// (api/search.go).
//
// No index is added for this read, and in particular no partial
// `embeddings (kind, id) where project_key is null`. The keyset is the table's own primary
// key, so a batch is one descent of embeddings_pkey and exactly $3 entries of it wherever the
// cursor stands - the plan test reads 200 entries at the start of the table and 200 from the
// middle of it. What a partial index buys a backfill is the labelled rows a late batch walks
// past, and a keyset leaves none to walk past: every row the pass reads it leaves behind it.
// The rows it reads and does not write are the ones stored since 20261109120000, which the
// write functions labelled themselves, and those are spread over the whole key range rather
// than banked above the cursor. Against nothing it would cost a second index built over
// 1,104,667 rows under the ACCESS EXCLUSIVE lock of a migration (20261109120000's header), an
// entry to maintain on every row written for as long as it existed, and an index that shrinks
// to empty as the pass it serves finishes.
//
// `labelled` writes only where project_key is still null, so a row written since 20261109120000
// - which the write functions label themselves - is read and left alone rather than rewritten,
// and a second run of the pass over the same keys writes nothing. The value is
// embeddings_owner_project_key's, the one definition of the project a row belongs to
// (20261109120000). A row whose owner is gone resolves to null, stays null and is passed over
// here for ever: nothing can label it, no join would have returned it to a scoped search
// either, and the block index deletes such a window when it next sees its owner.
//
// The cursor comes from `batch` and never from `labelled`'s `returning`: a row already labelled
// returns nothing, so a cursor taken from the write would stall on the first such row. `done`
// is set when the batch took no further row, which is the pass finished; the cursor then stays
// where it is.
//
// `batch` is read once and kept. Two references to a CTE already mean `as materialized`, which
// is written out here because the defect this statement replaced was a side the planner read
// again for every candidate row, and an edit that left this CTE with one reference would
// quietly inline it into the two statements that read it.
const fillEmbeddingProjectKeysBatchQuery = `
	with batch as materialized (
		select e.kind, e.id
		  from embeddings e
		 where (e.kind, e.id) > ($1::text, $2::text)
		 order by e.kind, e.id
		 limit $3
	), last as (
		select b.kind, b.id from batch b order by b.kind desc, b.id desc limit 1
	), labelled as (
		update embeddings e
		   set project_key = embeddings_owner_project_key(e.kind, e.id)
		  from batch b
		 where e.kind = b.kind and e.id = b.id and e.project_key is null
		returning 1
	), moved as (
		update embeddings_project_key_backfill
		   set kind = coalesce((select kind from last), embeddings_project_key_backfill.kind),
		       id = coalesce((select id from last), embeddings_project_key_backfill.id),
		       done = not exists (select 1 from last)
		returning done
	)
	select (select count(*) from labelled), (select done from moved)`

// FillEmbeddingProjectKeys labels the embeddings rows stored before 20261109120000 added
// project_key, and answers how many it labelled. The boot starts it in the background under a
// lease (api.RunEmbeddingProjectKeyFill): the pass is about an hour on a deployment of a
// million vectors, and an unlabelled row costs a project-scoped meaning search exactly what it
// cost that search before this release, so the server serves while it runs and every batch can
// only add rows such a search finds.
//
// A pass already finished costs one row: the progress row 20261109120100 creates says so, and
// this reads nothing else. Otherwise it loops, each iteration one short transaction that takes
// the next batch by keyset, labels it and commits, so no row it writes stays uncommitted for
// longer than its own batch and an enqueue that meets one of its keys waits for that batch
// alone. 20261109120100's header holds the measurement that says why.
//
// What it logs, which is the whole of what a reader of a rolling deploy sees of an hour-long
// pass: one line that it began and the cursor it begins from, one every
// embeddingProjectKeyProgressEvery while it runs, with the rows and batches so far, and one that
// it finished, with the rows, the batches and how long it took. A pass with nothing to do says so
// in the one line that costs it, and the lines are distinct sentences rather than one sentence in
// two tenses, so a reader of a stopped deployment's log can tell a fill still running from a fill
// that finished.
func (s *Store) FillEmbeddingProjectKeys(ctx context.Context) (int64, error) {
	var fromKind, fromID string
	var done bool
	if err := s.Pool.QueryRow(ctx, "select kind, id, done from embeddings_project_key_backfill").
		Scan(&fromKind, &fromID, &done); err != nil {
		return 0, fmt.Errorf("read the embeddings project_key fill's progress: %w", err)
	}
	if done {
		slog.Info("dispatch: the embeddings project_key fill has nothing to do: every row stored before this release carries its project")
		return 0, nil
	}
	// The cursor the pass starts from, which is the empty pair on a database no pass has reached
	// yet and the last row of the last committed batch on one a killed process left part-labelled:
	// the reader of the second boot's log sees it resume rather than start again.
	started := time.Now()
	slog.Info("dispatch: the embeddings project_key fill began",
		"from_kind", fromKind, "from_id", fromID, "batch", EmbeddingProjectKeyBatch)
	var labelled, batches int64
	said := started
	for {
		rows, finished, err := s.fillEmbeddingProjectKeysBatch(ctx)
		if err != nil {
			return labelled, err
		}
		labelled += rows
		batches++
		if finished {
			slog.Info("dispatch: the embeddings project_key fill finished",
				"rows", labelled, "batches", batches, "duration", time.Since(started))
			return labelled, nil
		}
		// Timed from the last line rather than counted in batches, so the rate of the lines holds
		// whatever the database's own rate of rows is: a batch is about a second on the deployment
		// the measurements come from, and a tenth of that on an empty test database.
		if now := time.Now(); now.Sub(said) >= embeddingProjectKeyProgressEvery {
			said = now
			slog.Info("dispatch: the embeddings project_key fill is labelling",
				"rows", labelled, "batches", batches, "elapsed", now.Sub(started))
		}
	}
}

// fillEmbeddingProjectKeysBatch runs one batch in a transaction of its own and commits it,
// answering the rows it labelled and whether the batch took no further row, which is the pass
// finished.
//
// Two statements, one transaction: the cursor is read and locked first and the batch is then
// planned against its values. Both properties the one statement had are the transaction's and
// not the statement's - the row lock is held to the commit, and the batch and the cursor it
// moves commit together or not at all - so splitting it costs a round trip to a one-row table
// and buys the plan fillEmbeddingProjectKeysCursorQuery's comment measures.
func (s *Store) fillEmbeddingProjectKeysBatch(ctx context.Context) (int64, bool, error) {
	// A batch is a transaction like any other, so it is marked like any other: nothing it runs
	// may take a second pooled connection while it is open.
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin an embeddings project_key fill batch: %w", err)
	}
	defer tx.Rollback(ctx)
	var fromKind, fromID string
	if err := tx.QueryRow(ctx, fillEmbeddingProjectKeysCursorQuery).Scan(&fromKind, &fromID); err != nil {
		return 0, false, fmt.Errorf("take the embeddings project_key fill's cursor: %w", err)
	}
	var labelled int64
	var finished bool
	if err := tx.QueryRow(ctx, fillEmbeddingProjectKeysBatchQuery, fromKind, fromID, EmbeddingProjectKeyBatch).
		Scan(&labelled, &finished); err != nil {
		return 0, false, fmt.Errorf("label a batch of embeddings rows with their project: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit an embeddings project_key fill batch: %w", err)
	}
	return labelled, finished, nil
}
