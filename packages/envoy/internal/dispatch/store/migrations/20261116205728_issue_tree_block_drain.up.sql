-- 20261116205728_issue_tree_block_drain.up.sql
--
-- How far the one drain of dispatch://INFRA-1253 has got, and nothing else: one row holding the
-- last document it marked and whether it is done.
--
-- This child's block index states, for every block of every version it indexes, the block's rank
-- among its siblings, its text and its placement, and hangs the top-level blocks of an issue's
-- spec, Verification and plan documents under the issue rather than under the document
-- (refs.IndexBlocks). A block a build before this child indexed holds none of those, so every
-- document stored before it owes its block index once more. A pass of the graph backfill
-- (graphcensus.Backfill, the drain in census_blocks.go) marks each stored document owed with this
-- release's own mark, document_block_versions.store_owed_at (20261116221539), which a release
-- before this one neither reads nor clears, and not the null version rebuild-refs sets, which such
-- a release drains as its own debt while it states none of those facts. The block-index
-- reconciliation indexes each, which writes those facts, clears the mark and moves no block's
-- changed_version and no document_block_texts row. The backfill runs at every boot, so this row
-- is what keeps the drain to one: a run that finds it done marks nothing.
--
-- The drain marks the documents in batches, each its own transaction under the documents' owner
-- rows, so a version write waits for one batch and not for every document. The cursor is the
-- document id the last committed batch ended at, null before the first, so a run stopped between
-- two batches starts again where it stopped. The drain inserts the row itself and reads it
-- `for update` in every batch, so two processes that boot together take turns rather than both
-- marking the same documents.
--
-- One new table: the release before this one neither reads nor writes it, nothing existing is
-- read, locked or rewritten, and no stored row can violate it.
create table issue_tree_block_drain (
  singleton boolean primary key default true check (singleton),
  after uuid,
  done boolean not null default false
);
