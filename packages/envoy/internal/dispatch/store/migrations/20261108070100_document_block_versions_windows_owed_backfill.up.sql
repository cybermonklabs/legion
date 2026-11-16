-- 20261108070100_document_block_versions_windows_owed_backfill.up.sql — the document window
-- marker's one backfill, as 20261107203841 is the items'.
--
-- 20261108070000 adds windows_owed_at to document_block_versions and has the version write and
-- 0122's trigger set it from then on. That marks nothing already stored: a document whose
-- windows stopped standing for the blocks the index holds before the column existed is marked
-- by no later write, so the window arm would pass over it for ever. This marks every such
-- document once, and the predicate it marks by is the arm's own (refs.DocumentsOwedWindows,
-- refs/blocks.go:463): the document's windows no longer stand - no stamp at the version its
-- blocks are indexed at (embedding_window_states, 20261104010400), or no window row at all -
-- and the arm would take it, which is a document the block index has reached (its row carries
-- the version its blocks are indexed at) or one holding a block.
--
-- `v.version is not null or held.block is not null`, and not the block alone. A version holding
-- no block is the arm's too - an issue created with no spec carries a spec.md whose markdown is
-- empty, so the index holds no document_blocks row for it - and the arm windows such a document
-- to no window, which deletes whatever windows a version that did hold blocks left behind and
-- clears the marker in the one statement (refs.WriteDocumentWindows). A backfill that required a
-- block would mark no such document, and nothing else ever marks one, so its stale windows would
-- outlive this upgrade for ever: measured as UPDATE 0 for a document in that state (INFRA-988).
--
-- A migration of its own, because it is the only statement of the pair whose cost grows with
-- the deployment and a migration is one transaction. Run beside the column it would hold
-- document_block_versions' ACCESS EXCLUSIVE - taken by that migration's ALTER and held to its
-- commit - for its whole read and update, and every version write upserts into that table
-- through 0122's trigger. Alone, an UPDATE takes ROW EXCLUSIVE: a writer of another
-- document's row does not wait at all, and one whose row this statement has already marked
-- waits for the commit.
--
-- It is written as the anti-joins and the semi-join rather than as correlated `exists` and `not
-- exists` probes under an `or`, as the items' backfill is and for the same reason: that is the
-- cheaper of the two readings of one predicate (INFRA-988). The table is small - 9,881 rows on
-- the load-test deployment, fewer in production - so this is one pass of it with three hash
-- joins.
--
-- The marker is empty for the already-stale documents between 20261108070000 and this
-- migration. Nothing reads it there that did not already behave that way: those documents have
-- had no window written since they went stale, so an arm that does not list them leaves them
-- exactly as the release before this one did, and this migration ends it.
--
-- It rewrites no value any reader had before it, and `windows_owed_at is null` is what holds it
-- to that: a document already marked - by a write since the column arrived, or by a run of this
-- statement - keeps the moment it carries, so running this twice marks no row a second time, and
-- a marker is read as a time, so re-stamping it would be a rewrite. The rows it does mark carry
-- null and are work owed rather than a rewrite, so its census answers 0. Its own lock waits are
-- bounded as every migration's are (pgmigrate.LockTimeout): a writer holding a row it wants for
-- five seconds fails that attempt, which the runner then makes again.
--
-- This file was edited after it had been applied, which a runner never re-reads (it records a
-- migration by its version, store.Migrate), so an edit reaches only databases that have not
-- applied it. That is safe here and only here: this migration has never been in a release - it
-- is added by the pull request that corrects it - so the only databases that ran the text before
-- this one were a load-test deployment, since deleted, and test databases, which are built again
-- from the whole set. A migration that had shipped would need a later one that re-runs it
-- instead, as 0044 does for 0043.
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
  left join (
    select b.artifact_id from document_blocks b group by 1
  ) held on held.artifact_id = d.artifact_id
  where d.windows_owed_at is null
    and (d.version is not null or held.artifact_id is not null)
    and (w.owner_key is null or e.owner_key is null)
) owed where v.artifact_id = owed.artifact_id;
