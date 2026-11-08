-- 20261108070100_document_block_versions_windows_owed_backfill.up.sql — the document window
-- marker's one backfill, as 20261107203841 is the items'.
--
-- 20261108070000 adds windows_owed_at to document_block_versions and has the version write and
-- 0122's trigger set it from then on. That marks nothing already stored: a document whose
-- windows stopped standing for the blocks the index holds before the column existed is marked
-- by no later write, so the window arm would pass over it for ever. This marks every such
-- document once, from the predicate the arm read before the marker: the document's blocks are
-- in the index, and either no stamp stands at the version those blocks are indexed at
-- (embedding_window_states, 20261104010400) or the document holds no window row at all.
--
-- A migration of its own, because it is the only statement of the pair whose cost grows with
-- the deployment and a migration is one transaction. Run beside the column it would hold
-- document_block_versions' ACCESS EXCLUSIVE - taken by that migration's ALTER and held to its
-- commit - for its whole read and update, and every version write upserts into that table
-- through 0122's trigger. Alone, an UPDATE takes ROW EXCLUSIVE: a writer of another
-- document's row does not wait at all, and one whose row this statement has already marked
-- waits for the commit.
--
-- It is written as anti-joins rather than as the two correlated `not exists` probes under an
-- `or`, as the items' backfill is and for the same reason: that is the cheaper of the two
-- readings of one predicate (INFRA-988). The table is small - 9,881 rows on the load-test
-- deployment, fewer in production - so this is one pass of it with two hash joins.
--
-- The marker is empty for the already-stale documents between 20261108070000 and this
-- migration. Nothing reads it there that did not already behave that way: those documents have
-- had no window written since they went stale, so an arm that does not list them leaves them
-- exactly as the release before this one did, and this migration ends it.
--
-- It rewrites no value any reader had before it: windows_owed_at is read by this release alone
-- (refs.DocumentsOwedWindows), and the rows it marks are work owed, not a rewrite, so its
-- census answers 0. Its own lock waits are bounded as every migration's are
-- (pgmigrate.LockTimeout): a writer holding a row it wants for five seconds fails that
-- attempt, which the runner then makes again.
update document_block_versions v set windows_owed_at = now() from (
  select d.artifact_id from document_block_versions d
  left join embedding_window_states w
         on w.kind = 'document_block'
        and w.owner_key = d.artifact_id::text
        and w.block_state = coalesce(d.version::text, '')
  left join (
    select split_part(e.id, '#', 1) as owner_key from embeddings e
    where e.kind = 'document_block'
      and e.kind in ('document_block', 'comment_block', 'message_block', 'ask_part')
    group by 1
  ) e on e.owner_key = d.artifact_id::text
  where exists (select 1 from document_blocks b where b.artifact_id = d.artifact_id)
    and (w.owner_key is null or e.owner_key is null)
) owed where v.artifact_id = owed.artifact_id;
