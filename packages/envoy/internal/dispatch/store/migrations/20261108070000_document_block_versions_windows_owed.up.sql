-- 20261108070000_document_block_versions_windows_owed.up.sql — the document half of the
-- embedding-window arm's marker, as 20261107194001 is the items'.
--
-- The block-index tick's window arm lists the documents whose windows do not stand for the
-- blocks the index holds (refs.DocumentsOwedWindows, docs.windowDrift). It asked that of state,
-- once per row of document_block_versions: whether a stamp stands at the version those blocks
-- are indexed at (embedding_window_states, 20261104010400) and whether the document holds a
-- window row at all, two index probes a row. Measured on the load-test deployment at 9,881
-- document_block_versions rows, 1,008,839 document_blocks and 382,843 document-block
-- embeddings, EXPLAIN (ANALYZE, BUFFERS) of that statement answers no rows in 112-122 ms with
-- 67,496 buffer hits: the same O(table) shape that cost the comment listing 13.7 s, over a
-- table fifty times smaller, and a tick runs it every 2 s while the arm has a full batch
-- (INFRA-988).
--
-- It will not get cheaper. Since the version write stopped writing a document's windows
-- (refs.IndexBlocks), every edited document answers that read rather than none.
--
-- windows_owed_at is that question written down, in the shape the items already have: set
-- where the document's windows stop standing for its blocks, cleared only by the statement
-- that writes those windows (refs.WriteDocumentWindows, beside the stamp, so the clear and the
-- write cannot come apart), and read through the partial index below, so the arm reads the
-- owed rows rather than every row of the table.
--
-- What sets it:
--
--   - the version write itself, in the transaction that writes the blocks those windows will
--     cover (refs.IndexBlocks, in the upsert of this row that records the version its blocks
--     are indexed at). No trigger is needed for that path, and a crash between the two leaves
--     the document owed rather than silently windowless.
--   - this migration's rewrite of 0122's document_block_versions_owe, on every version any
--     build inserts, for the reason 20261107194001 gives for the items' trigger: the release
--     before this one writes a version, indexes its blocks and clears nothing it does not
--     know, which is the rolling-deploy case, so the trigger marks the document for this
--     release's arm and that build's index cannot unmark it.
--   - rebuild-refs, which re-derives every document from its stored text (refs.RebuildAll
--     marks the block index owed by nulling the version, and the index that answers it marks
--     the windows owed here).
--   - 20261108070100, which marks once every document whose windows were already stale when
--     this column arrived, and which no later write would have marked.
--
-- The version is no answer to this question on its own: that is what the stamp comparison was,
-- the read this column replaces.
--
-- Nothing else removes a document's window row: refs.writeWindows' own `dropped` is the
-- server's only delete of one, and it is in the statement that writes the new set and clears
-- this marker. A route that removes one later marks its owner here, or the arm will not see
-- it.
--
-- Rolling deploy. The column is additive and nullable, no earlier release names it, and the
-- release before this one reads and writes every column it knows as before: its insert of a
-- row through 0122's trigger leaves the new column null, and its own index of a version
-- leaves it as the trigger set it. A document that release windows itself is left marked and
-- this release's arm rewrites its windows once, which is a no-op upsert for every window whose
-- text did not move (refs.writeWindows).
--
-- The ALTER takes ACCESS EXCLUSIVE on document_block_versions and the CREATE INDEX SHARE, both
-- over a table of 9,881 rows on the load-test deployment and fewer in production; the index is
-- built over a column that is null in every row, so it is built empty. The backfill is a
-- migration of its own (20261108070100), as the items' is, so this transaction's ACCESS
-- EXCLUSIVE is not held for a read whose cost grows with the deployment.
alter table document_block_versions add column windows_owed_at timestamptz;

create or replace function document_block_versions_owe() returns trigger language plpgsql as $$
begin
  insert into document_block_versions (artifact_id, version, windows_owed_at)
  values (new.artifact_id, null, now())
  on conflict (artifact_id) do update
    set version = null,
        windows_owed_at = coalesce(document_block_versions.windows_owed_at, excluded.windows_owed_at);
  return null;
end;
$$;

create index document_block_versions_windows_owed
  on document_block_versions (artifact_id) where windows_owed_at is not null;
