package store

import (
	"context"
	"fmt"
	"log/slog"
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

// fillEmbeddingProjectKeysBatchQuery is one batch: it takes the next EmbeddingProjectKeyBatch
// rows by keyset from the cursor, labels each one whose project_key is still null from the
// owner the row belongs to, moves the cursor to the last row it took, and answers the rows it
// labelled and whether the pass is finished. $1 is the batch size.
//
// The cursor is read `for update`, which is what makes two processes filling at once take
// different batches rather than the same one: the second waits for the first's batch to commit
// and then reads the cursor it moved.
//
// `batch` is the keyset: the next rows above the cursor in (kind, id) order, which is
// embeddings' own primary key, so the scan starts where the last batch stopped instead of
// counting past it as an `offset` would. It takes the rows of every kind, the issue's whole-text
// rows among the four window kinds, because every arm of meaning search now reads the column
// (api/search.go).
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
const fillEmbeddingProjectKeysBatchQuery = `
	with progress as (
		select kind, id from embeddings_project_key_backfill for update
	), batch as (
		select e.kind, e.id
		  from embeddings e, progress
		 where (e.kind, e.id) > (progress.kind, progress.id)
		 order by e.kind, e.id
		 limit $1
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
// project_key, and answers how many it labelled. The boot runs it in the background
// (cmd/dispatch/main.go): the pass is about an hour on a deployment of a million vectors, and
// an unlabelled row costs a project-scoped meaning search exactly what it cost that search
// before this release, so the server serves while it runs and every batch can only add rows
// such a search finds.
//
// A pass already finished costs one row: the progress row 20261109120100 creates says so, and
// this reads nothing else. Otherwise it loops, each iteration one short transaction that takes
// the next batch by keyset, labels it and commits, so no row it writes stays uncommitted for
// longer than its own batch and an enqueue that meets one of its keys waits for that batch
// alone. 20261109120100's header holds the measurement that says why.
func (s *Store) FillEmbeddingProjectKeys(ctx context.Context) (int64, error) {
	var done bool
	if err := s.Pool.QueryRow(ctx, "select done from embeddings_project_key_backfill").Scan(&done); err != nil {
		return 0, fmt.Errorf("read the embeddings project_key fill's progress: %w", err)
	}
	if done {
		return 0, nil
	}
	var labelled, batches int64
	for {
		rows, finished, err := s.fillEmbeddingProjectKeysBatch(ctx)
		if err != nil {
			return labelled, err
		}
		labelled += rows
		batches++
		if finished {
			slog.Info("dispatch: embeddings project_key fill finished", "rows", labelled, "batches", batches)
			return labelled, nil
		}
	}
}

// fillEmbeddingProjectKeysBatch runs one batch in a transaction of its own and commits it,
// answering the rows it labelled and whether the batch took no further row, which is the pass
// finished.
func (s *Store) fillEmbeddingProjectKeysBatch(ctx context.Context) (int64, bool, error) {
	// A batch is a transaction like any other, so it is marked like any other: nothing it runs
	// may take a second pooled connection while it is open.
	ctx = WithTransactionTracking(ctx)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin an embeddings project_key fill batch: %w", err)
	}
	defer tx.Rollback(ctx)
	var labelled int64
	var finished bool
	if err := tx.QueryRow(ctx, fillEmbeddingProjectKeysBatchQuery, EmbeddingProjectKeyBatch).Scan(&labelled, &finished); err != nil {
		return 0, false, fmt.Errorf("label a batch of embeddings rows with their project: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit an embeddings project_key fill batch: %w", err)
	}
	return labelled, finished, nil
}
