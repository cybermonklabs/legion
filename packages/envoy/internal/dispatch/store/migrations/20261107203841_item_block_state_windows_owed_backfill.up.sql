-- 20261107203841_item_block_state_windows_owed_backfill.up.sql — the window marker's one
-- backfill.
--
-- 20261107194001 adds windows_owed_at to the three item state tables and has the trigger set it
-- on every write of an item's text from then on. That marks nothing already stored: an item
-- whose windows stopped standing for its blocks before the column existed is marked by no later
-- write, and the block index's own owed_at does not cover it, so the window arm would pass over
-- it for ever. This marks every such item once, from the predicate the arm read before the
-- marker: the item's blocks are in the index, and either no stamp stands at the state those
-- blocks are in now (embedding_window_states, 20261104010400) or the item holds no window row
-- at all.
--
-- A migration of its own, because it is the only statement of the pair whose cost grows with
-- the deployment and a migration is one transaction. Run beside the column it would have held
-- the three tables' ACCESS EXCLUSIVE - taken by that migration's first ALTER and held to its
-- commit - for its whole read and update: measured at 8.5 s on 511,595 comment state rows with
-- every row marked, which is 8.5 s in which no comment, message or ask can be written, since
-- the trigger upserts into those tables. Alone, an UPDATE takes ROW EXCLUSIVE: a writer of
-- another item's row does not wait at all, and one whose row this statement has already marked
-- waits for the commit. The same seconds then cost concurrency rather than availability.
--
-- Each statement is written as the anti-joins rather than as the two correlated `not exists`
-- probes under an `or`, because that is the cheaper of the two readings of one predicate: 2.92 s
-- against 13.7 s a pass at that size (INFRA-988).
--
-- The marker is empty for the already-stale items between 20261107194001 and this migration.
-- Nothing reads it there that did not already behave that way: those items have had no window
-- written since they went stale, so an arm that does not list them leaves them exactly as the
-- release before this one did, and this migration ends it. A new build that reaches this
-- database with only the first migration applied therefore under-lists and writes nothing
-- wrong, and a database holding both before that build serves is already settled.
--
-- It rewrites no value any reader had before it: windows_owed_at is read by this release alone
-- (refs.ItemsOwedWindows), and the rows it marks are work owed, not a rewrite, so its census
-- answers 0. Its own lock waits are bounded as every migration's are (pgmigrate.LockTimeout):
-- a writer holding a state row it wants for five seconds fails that attempt, which the runner
-- then makes again.
update comment_block_state s set windows_owed_at = now() from (
  select d.comment_id from comment_block_state d
  left join embedding_window_states w
         on w.kind = 'comment_block'
        and w.owner_key = 'comment/' || d.comment_id::text
        and w.block_state = coalesce(d.body_sha256, '')
  left join (
    select split_part(e.id, '#', 1) as owner_key from embeddings e
    where e.kind = 'comment_block'
      and e.kind in ('document_block', 'comment_block', 'message_block', 'ask_part')
    group by 1
  ) e on e.owner_key = 'comment/' || d.comment_id::text
  where exists (
    select 1 from comment_blocks b where b.comment_id = d.comment_id and b.position is not null
  )
    and (w.owner_key is null or e.owner_key is null)
) owed where s.comment_id = owed.comment_id;

update message_block_state s set windows_owed_at = now() from (
  select d.message_id from message_block_state d
  left join embedding_window_states w
         on w.kind = 'message_block'
        and w.owner_key = 'message/' || d.message_id::text
        and w.block_state = coalesce(d.body_sha256, '')
  left join (
    select split_part(e.id, '#', 1) as owner_key from embeddings e
    where e.kind = 'message_block'
      and e.kind in ('document_block', 'comment_block', 'message_block', 'ask_part')
    group by 1
  ) e on e.owner_key = 'message/' || d.message_id::text
  where exists (
    select 1 from message_blocks b where b.message_id = d.message_id and b.position is not null
  )
    and (w.owner_key is null or e.owner_key is null)
) owed where s.message_id = owed.message_id;

update ask_block_state s set windows_owed_at = now() from (
  select d.ask_id from ask_block_state d
  left join embedding_window_states w
         on w.kind = 'ask_part'
        and w.owner_key = 'ask/' || d.ask_id::text
        and w.block_state = coalesce(d.body_sha256, '')
  left join (
    select split_part(e.id, '#', 1) as owner_key from embeddings e
    where e.kind = 'ask_part'
      and e.kind in ('document_block', 'comment_block', 'message_block', 'ask_part')
    group by 1
  ) e on e.owner_key = 'ask/' || d.ask_id::text
  where exists (
    select 1 from ask_blocks b where b.ask_id = d.ask_id and b.position is not null
  )
    and (w.owner_key is null or e.owner_key is null)
) owed where s.ask_id = owed.ask_id;
