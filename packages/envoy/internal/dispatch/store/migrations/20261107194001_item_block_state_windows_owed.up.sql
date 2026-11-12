-- 20261107194001_item_block_state_windows_owed.up.sql — the embedding-window arm's own marker.
--
-- The block-index tick's window arm lists the comments, messages and asks whose windows do not
-- stand for the blocks the index holds (refs.ItemsOwedWindows, docs.windowDrift). It asked that
-- of state, once per row of the kind's state table: whether a stamp stands at the state those
-- blocks are in now (embedding_window_states, 20261104010400) and whether the item holds a
-- window row at all. The rows it looks for are rare exactly when the arm has nothing to do, so
-- the whole table was read to find none: 511,595 comment state rows, 1,023,190 index searches,
-- 5.6 million buffer hits, 13.7 s a pass, 200 passes an hour, 6.5% of the load-test
-- deployment's database time, for an answer of no rows (INFRA-988). Written as anti-joins the
-- same question is 2.92 s a pass and writes 472 MB of temporary files, which is better and
-- still a whole-table read.
--
-- windows_owed_at is that question written down: set where the window state stops standing for
-- the blocks, cleared only by the statement that writes the windows (refs.writeWindows, beside
-- the stamp, so the clear and the write cannot come apart), and read through the partial index
-- below, so the arm reads the owed rows rather than every row of the state table.
--
-- owed_at cannot serve. It is the block index's marker: this function sets it on every write of
-- an item's text and the index clears it (refs.recordItemIndexed), which leaves this column
-- standing, since that index writes no window of its own. "A block index is owed" and
-- "the windows do not stand for the blocks the index holds now" are nearly disjoint sets, so an
-- arm reading owed_at would take the block arm's backlog and leave a genuinely drifted item
-- unwindowed for ever.
--
-- What sets it:
--
--   - this trigger function, on every write of the text an item's blocks are split from, beside
--     owed_at and for its reason: the write moves the blocks, so windows written from the text
--     before it no longer stand for them. The trigger rather than the server's write sites
--     because the release before this one writes an item's text, indexes its blocks and clears
--     owed_at without knowing this column — the rolling-deploy case the stamp comparison was
--     added for (20261104010400). The trigger runs for that release too, and its index cannot
--     clear a column it does not name, so the item is left marked and this release's arm
--     windows it. As with owed_at, a write landing on a state row that already exists moves
--     only the two markers, never its revision or next_block, so a pass that read the earlier
--     time leaves the row owed.
--   - rebuild-refs (refs.RebuildAll), which marks every item owed a block index and re-derives
--     each one from its stored text, its windows included.
--   - 20261107203841, which marks once every item whose windows were already stale when this
--     column arrived. That backfill is a migration of its own because it is the one statement
--     here whose cost grows with the deployment, and a migration is one transaction: run here
--     it would hold the ACCESS EXCLUSIVE below for its whole read and update. See its header.
--
-- Nothing else removes an item's window row: writeWindows' own `dropped` is the server's only
-- delete of one, and it is in the statement that writes the new set and clears this marker. A
-- route that removes one later marks its owner here, or the arm will not see it.
--
-- Rolling deploy, and the window between this migration and its backfill. The column is
-- additive and nullable, no earlier release names it, and the release before this one reads and
-- writes every column it knows as before: its insert of a state row leaves the new column null,
-- and its index clears owed_at and leaves this marker as the trigger set it, which is what this
-- release's arm is for. Between this migration and 20261107203841 the marker is null for every
-- item whose windows were already stale, so the arm lists none of them - which is what the arm
-- did before this release shipped at all, since nothing had windowed them since they went
-- stale - and the backfill ends it. Both orders of the pair against the code are therefore
-- safe: a build that reads the marker before the backfill has applied under-lists and writes
-- nothing wrong, and a database that has both before the new build serves is already settled.
-- Rolled back, the column and the trigger stay, the marker accumulates unread, and the earlier
-- release's own predicate answers as it did.
--
-- Locks and sizes. One transaction, so the three state tables are held ACCESS EXCLUSIVE from
-- the first ALTER to the commit - but every statement here is a catalog change or an index
-- build over no row: the ALTERs rewrite nothing (3.7 ms each, measured on 511,595 rows) and
-- each index is partial on a column that is null in every row, so it is built empty. The whole
-- migration is under a second at that size. The tables are small besides: 81 MB at 511,595
-- rows, of which 62 MB heap and 19 MB indexes, far under the census's 1 GiB limit, and
-- production holds about a tenth of that count.
alter table comment_block_state add column windows_owed_at timestamptz;
alter table message_block_state add column windows_owed_at timestamptz;
alter table ask_block_state add column windows_owed_at timestamptz;

create or replace function item_blocks_owe() returns trigger language plpgsql as $$
begin
  if tg_argv[0] = 'comment' then
    insert into comment_block_state (comment_id, owed_at, windows_owed_at) values (new.id, now(), now())
    on conflict (comment_id) do update set owed_at = excluded.owed_at, windows_owed_at = excluded.windows_owed_at;
  elsif tg_argv[0] = 'message' then
    insert into message_block_state (message_id, owed_at, windows_owed_at) values (new.id, now(), now())
    on conflict (message_id) do update set owed_at = excluded.owed_at, windows_owed_at = excluded.windows_owed_at;
  elsif tg_argv[0] = 'ask' then
    insert into ask_block_state (ask_id, owed_at, windows_owed_at) values (new.id, now(), now())
    on conflict (ask_id) do update set owed_at = excluded.owed_at, windows_owed_at = excluded.windows_owed_at;
  else
    raise exception 'item_blocks_owe: unknown item kind %', tg_argv[0];
  end if;
  return null;
end;
$$;

create index comment_block_state_windows_owed on comment_block_state (comment_id) where windows_owed_at is not null;
create index message_block_state_windows_owed on message_block_state (message_id) where windows_owed_at is not null;
create index ask_block_state_windows_owed on ask_block_state (ask_id) where windows_owed_at is not null;
