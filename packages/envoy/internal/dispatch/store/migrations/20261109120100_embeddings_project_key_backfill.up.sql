-- 20261109120100_embeddings_project_key_backfill.up.sql — how far the labelling of the rows
-- stored before 20261109120000 has got, and nothing else: one row holding the keyset cursor
-- and whether the pass is finished.
--
-- 20261109120000 adds project_key and has both write functions set it from the row's owner from
-- then on. That labels nothing already stored, and an unlabelled row is a row no
-- project-scoped meaning search can return - which is the defect this change exists to end, so
-- the stored rows have to be labelled once. The pass itself is
-- store.FillEmbeddingProjectKeys, which the boot runs in committed batches
-- (cmd/dispatch/main.go), and this migration is the one row it records its place in, as
-- 20261101182501 is for document_search's fill.
--
-- It is batched, and so it is not one migration statement, because the work is an hour of row
-- rewrites. Each embeddings row carries a 1,536-float vector, so an update rewrites about 6 KB
-- of it: labelling 120,000 rows of the load-test deployment took 12 m 50 s and 19,650 took
-- 86 s, which is 156 to 228 rows a second, and a single-statement backfill of its 723,015
-- comment windows had not finished after 30 minutes (INFRA-988). A migration is one
-- transaction (store/store.go, applyMigrationOnce): run here, that hour would be an hour in
-- which every row the statement had already touched is locked - a comment edit whose window
-- the pass has passed waits for all of it - and an hour of a deploy's migration step. In
-- committed batches of store.EmbeddingProjectKeyBatch (200 rows, about a second at the rates
-- above) such a writer waits for one batch.
--
-- The cursor is the table's own primary key, (kind, id), so each batch starts where the last
-- one stopped instead of counting past it as an `offset` would, and the empty pair sorts below
-- every row. done is what an ordinary boot reads and stops on, so a boot after the pass has
-- finished costs this one row and no read of embeddings at all.
--
-- One row, as 20261101182501's cursor is one row, and the same singleton primary key keeps it
-- one. Two processes filling at once - both releases of a rolling deploy at their boots - take
-- different batches rather than the same one: the fill reads this row `for update`, so the
-- second waits for the first's batch to commit and then reads the cursor it moved.
--
-- Rolling deploy. The column is nullable and this release alone reads it, so a database that
-- has this migration and not the fill behaves exactly as the release before it did: an
-- unlabelled row is returned by an unscoped meaning search, which reads no project predicate
-- at all, and left out of a scoped one, which is what that search did with it before this
-- release shipped. The pass only ever moves rows from the second case to the first.
--
-- This migration creates one table and inserts its one row. It reads no large table, takes no
-- lock that grows with any table, and commits in milliseconds; its census answers 0.
create table embeddings_project_key_backfill (
  singleton boolean primary key default true check (singleton),
  kind text not null default '',
  id text not null default '',
  done boolean not null default false
);
insert into embeddings_project_key_backfill default values;
