-- 20261116170000_read_state_markers.up.sql
--
-- The five read-state tables other than user_threads learn since when the node and tuple store
-- holds the mark their row says (INFRA-1152's D4 and R6): user_agent_read, user_agent_state,
-- user_agent_reply_read, user_agent_thread_dismissed and user_ask_snooze each take
--   tuples_stored_at   the moment of the mark the store holds for the row, which is its tuple's
--                      own moment, set by the statement of this release's writer that writes the
--                      row, in the transaction that appends the tuple from the person's node
--                      (graphstore.FromPerson): read_through, cleared_before and dismissed_at as
--                      the row stores them, snoozed_until for a snooze (decision 13), and the
--                      moment of the reading for a reply read by id, which stores no moment of its
--                      own. Null for a row an older build wrote, and for one whose person or far
--                      end lies past the store's address bound, which states no tuple.
-- A row is marked where tuples_stored_at is not null. The name and the type are asks' marker's
-- (20261116140001), the branch's other nullable marker: a moment rather than a flag, because the
-- moment is what tells which tuple the row projects, and for a reply read by id it is the one
-- record of when the reading was. user_threads' mark is two figures instead (marked_version and
-- marked_seq, 20261116160000), because its version is counted from the deliveries since the mark,
-- and none of these five holds a count.
--
-- A marker survives only this release's writer, as 20261116150000's and user_threads' do: every
-- connection of this release's pool carries dispatch.store_marker_writer `on`
-- (store.StoreMarkerWriterSetting), and an update of a marked row from anywhere else - an older
-- build's route moving a read mark, a Clear, a dismissal or a snooze - states nothing in the
-- store, so the trigger below, before each update, takes the marker off the row it writes: the row
-- returns to its old arm, and the backfill takes it again. No trigger body of either release
-- updates these tables (the clear of 20261105093507 deletes), so the setting alone decides, with
-- no test of the nesting depth as user_threads' trigger needs. The function reads no table and
-- writes no row of its own, so it adds no step to any writer's lock order.
--
-- Each column is nullable with no default: a catalog change, so no stored row is rewritten or
-- read, and every existing row reads null, which is true of it, since no writer before this one
-- stated a row's mark as a tuple. Each ALTER takes ACCESS EXCLUSIVE on its table for its catalog
-- write and `create trigger` the SHARE ROW EXCLUSIVE it already holds, and the migration holds
-- each table's lock until it commits, so it holds the first table's while it waits for the next.
-- A write or a read of the release still serving takes these tables in another order -
-- putUserAgentState writes user_agent_state before user_agent_read, the agent state read takes
-- all three of its tables in one statement - so the two can each hold what the other waits for.
-- Half a second, shorter than deadlock_timeout (1 s by default), makes this migration give up
-- first, as 20261116150000 does for the same cycle over the tables it names: it fails with 55P03
-- having applied nothing, the write finishes, and the runner applies it again
-- (pgmigrate.LockTimeoutAttempts). The release before this one names its columns in every read
-- and write of these tables, so the two roll side by side; rolling back past this migration is an
-- image rollback alone, and the columns left behind are read by nothing of that release.
set local lock_timeout = '500ms';

alter table user_agent_read add column tuples_stored_at timestamptz;
alter table user_agent_state add column tuples_stored_at timestamptz;
alter table user_agent_reply_read add column tuples_stored_at timestamptz;
alter table user_agent_thread_dismissed add column tuples_stored_at timestamptz;
alter table user_ask_snooze add column tuples_stored_at timestamptz;

create function read_state_unmarked_by_older_writer() returns trigger language plpgsql as $$
begin
  if current_setting('dispatch.store_marker_writer', true) is distinct from 'on' then
    new.tuples_stored_at := null;
  end if;
  return new;
end;
$$;

create trigger user_agent_read_store_marker before update on user_agent_read
  for each row execute function read_state_unmarked_by_older_writer();

create trigger user_agent_state_store_marker before update on user_agent_state
  for each row execute function read_state_unmarked_by_older_writer();

create trigger user_agent_reply_read_store_marker before update on user_agent_reply_read
  for each row execute function read_state_unmarked_by_older_writer();

create trigger user_agent_thread_dismissed_store_marker before update on user_agent_thread_dismissed
  for each row execute function read_state_unmarked_by_older_writer();

create trigger user_ask_snooze_store_marker before update on user_ask_snooze
  for each row execute function read_state_unmarked_by_older_writer();
