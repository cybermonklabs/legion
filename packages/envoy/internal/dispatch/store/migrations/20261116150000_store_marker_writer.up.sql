-- 20261116150000_store_marker_writer.up.sql
--
-- A row this release marks keeps its marker only while this release's writer is the one that
-- writes it. The node pair (node_kind, node_address) of an attempt row (migration 20261116102800)
-- and of an item's block and link rows (20261116140000) says the row's writer stated what the row
-- holds in the node and tuple store, in the transaction that wrote it. An insert a build of the
-- release before this one makes names no node, so its row is answered by its old arm, and the
-- backfill takes it once. An update that build makes is the same: it writes the row's columns and
-- states nothing in the store - a send's state, envelope_id and claimed_at, a reply's reply_id, a
-- restored block's position - yet left alone it would keep the marker, and the node would go on
-- saying what the row no longer does, a difference no backfill reads, since the backfill walks
-- only the rows that name no node and nothing reconciles a marked one (R9). So an update that
-- build makes unmarks the row: it returns to its old arm, and the backfill takes it again, the
-- same contract as an insert that build makes. A restored block is the case that shows it: the
-- release before this one restores a removed block by an upsert that moves its position and leaves
-- its node columns as they were, over a node this release stated gone, and only this unmarking has
-- its old arm answer the row's part_of rather than neither arm.
--
-- The writer generation is a session setting:
--   dispatch.store_marker_writer   `on` on every connection of this release's pool, a startup
--                                  parameter the pool sets (store.StoreMarkerWriterSetting), so it
--                                  holds for the connection's whole life. A build of the release
--                                  before this one never sets it.
-- The trigger on each table whose rows this release marks and the release before it updates names
-- no node in the row an update writes where the setting is not `on`: comment_deliveries and
-- message_deliveries, comment_blocks, message_blocks and ask_blocks, and comment_block_refs,
-- message_block_refs and ask_block_refs. It runs before the update, and changes only the row the
-- update writes, so it writes no row of its own: it reads no table and adds no step to the writer's
-- lock order, and a row whose columns name no node already is written as it was. An update of this
-- release keeps the node pair the update itself gives the row, including a pair it clears. The
-- foreign key is MATCH SIMPLE, which holds no row whose columns are null, so the unmarked row is
-- checked against nothing. The other markers of this release - comments.tuples_stored,
-- refs.tuples_stored, messages' node pair and asks.tuples_stored_at - each mark facts of columns
-- no build updates (a comment's and a message's pointers, a refs row's key), or, for an ask's
-- answer, a moment an answer an older build writes is told apart by (20261116140001), so none of
-- them takes the trigger.
--
-- The read-state tables (INFRA-1152's T9) and user_threads' marked_version and marked_seq (T7)
-- take the same treatment when their markers land: a trigger of their own before each update,
-- clearing their marker where the setting is not `on`, through a function of their marker's shape
-- beside this one.
--
-- One function, for the one marker shape the eight tables share, and one trigger per table, all
-- in this one transaction. `create or replace function` takes no lock on any table. `create
-- trigger` takes SHARE ROW EXCLUSIVE on its table for the catalog write, which blocks that table's
-- writers and no reader, and the migration holds each table's until it commits, so it holds the
-- first table's while it waits for the next. A write of the release still serving takes these
-- tables in another order - a comment write inserts its attempts and then its blocks - so the two
-- can each hold what the other waits for. Half a second, shorter than deadlock_timeout (1 s by
-- default), makes this migration give up first, as 20261116140000 does for the same cycle over
-- the same six item tables: it fails with 55P03 having applied nothing, the write finishes, and
-- the runner applies it again (pgmigrate.LockTimeoutAttempts). No row is read or rewritten, and
-- the census answers 0.
set local lock_timeout = '500ms';

create or replace function store_marker_unmarked_by_older_writer() returns trigger language plpgsql as $$
begin
  if current_setting('dispatch.store_marker_writer', true) is distinct from 'on' then
    new.node_kind := null;
    new.node_address := null;
  end if;
  return new;
end;
$$;

create trigger comment_deliveries_store_marker before update on comment_deliveries
  for each row execute function store_marker_unmarked_by_older_writer();

create trigger message_deliveries_store_marker before update on message_deliveries
  for each row execute function store_marker_unmarked_by_older_writer();

create trigger comment_blocks_store_marker before update on comment_blocks
  for each row execute function store_marker_unmarked_by_older_writer();

create trigger message_blocks_store_marker before update on message_blocks
  for each row execute function store_marker_unmarked_by_older_writer();

create trigger ask_blocks_store_marker before update on ask_blocks
  for each row execute function store_marker_unmarked_by_older_writer();

create trigger comment_block_refs_store_marker before update on comment_block_refs
  for each row execute function store_marker_unmarked_by_older_writer();

create trigger message_block_refs_store_marker before update on message_block_refs
  for each row execute function store_marker_unmarked_by_older_writer();

create trigger ask_block_refs_store_marker before update on ask_block_refs
  for each row execute function store_marker_unmarked_by_older_writer();
