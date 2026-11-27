-- 20261116160000_user_threads_markers.up.sql
--
-- user_threads learns since when the node and tuple store holds what its row says (this child's D4
-- and R6):
--   marked_version   the row's version at the moment it was marked: 0 for a row this release's
--                    writer begins (inboxwrite.Record, a person's first write in a thread), the
--                    row's version as it stood for a row the backfill marks.
--   marked_seq       the log position (graph_tuples.seq) at that moment.
-- A row is marked where marked_seq is not null. On a marked row, version is marked_version plus
-- the delivered_to tuples into that person from that thread, with seq above marked_seq, that a
-- reply caused, which the census proves. One lock rule makes that hold: the writer locks every
-- row of the thread it writes in before it appends the delivered_to tuples of the people who hold
-- them, and the backfill marks a row under that same lock. A row not marked is answered by the old
-- trigger (user_threads_record) until the backfill takes it once.
--
-- A marker survives only this release's writer, as the node pair of the rows 20261116150000 names
-- does: every connection of this release's pool carries dispatch.store_marker_writer `on`
-- (store.StoreMarkerWriterSetting), and an update of a marked row from anywhere else is one whose
-- delivery no tuple of this release states - an older build's route statement, or the old trigger
-- body user_threads_record moving the version - so the trigger below, before each update, takes
-- the marker off the row it writes: the row returns to the backfill, which marks it again under
-- the lock above.
--
-- The setting alone does not find the old trigger body when it runs inside this release's own
-- transaction, whose connection carries the setting: this release still leaves one write to it -
-- an answer the boot step's decision step replays from another build's ask.answered (docs'
-- decision step, asks.Recorded), whose update leaves the ask's marker where it was - which moves
-- the version of a marked row with no delivered_to beside it, just as an older build's write
-- does. That body only ever runs inside the trigger of the write that fires it
-- (user_threads_comment, user_threads_message, user_threads_ask), so its update reaches this
-- trigger at a nesting depth above one (pg_trigger_depth), and the second test unmarks it
-- whichever build's transaction it runs in. Every statement this release's own writers run on
-- user_threads - inboxwrite.Record's moves and its own row, and the read-mark and dismissal
-- routes - runs at depth one, outside any trigger, and keeps the marker it finds; a comment or a
-- message past the store's address bound, which this release recorded through the triggers before,
-- is recorded in Go too, and that writer clears the marker of each row it moves in its own
-- statement, as the body's update did (inboxwrite's Write.Unstored).
--
-- Both columns are nullable with no default: a catalog change, so no stored row is rewritten or
-- read, and every existing row reads null, which is true of it, since no writer before this one
-- stated a row's tuples. The ALTER takes ACCESS EXCLUSIVE on user_threads for its catalog write and
-- `create trigger` the SHARE ROW EXCLUSIVE it already holds, and no lock on any other table, so it
-- closes no cycle with a write of the release still serving, which takes the item it writes and
-- then user_threads from its trigger; the runner's lock_timeout bounds its wait
-- (pgmigrate.LockTimeout), and the runner applies it again on a timeout
-- (pgmigrate.LockTimeoutAttempts). The release before this one names its columns in every read and
-- write of user_threads, so the two roll side by side; rolling back past this migration is an
-- image rollback alone, and the columns left behind are read by nothing of that release.
alter table user_threads add column marked_version bigint, add column marked_seq bigint;

-- kept for one release: older-writer-unmark
create function user_threads_unmarked_by_older_writer() returns trigger language plpgsql as $$
begin
  if current_setting('dispatch.store_marker_writer', true) is distinct from 'on' or pg_trigger_depth() > 1 then
    new.marked_version := null;
    new.marked_seq := null;
  end if;
  return new;
end;
$$;

-- The trigger's WHEN holds the function's own test, so the function is called only for an update
-- it unmarks: a reply moves the rows of every person in its thread, and calling the function for
-- each of a thousand rows this release's writer keeps cost 3.5 ms of a 999-row move on
-- 2026-11-21, where the expression costs a few microseconds in all. WHEN is evaluated before the
-- function is called, at the depth of the statement that updates the row, so its depth test is
-- `> 0` where the function body's, called one level deeper, is `> 1`: both say the update runs
-- inside another trigger.
-- kept for one release: older-writer-unmark
create trigger user_threads_store_marker before update on user_threads
  for each row
  when (current_setting('dispatch.store_marker_writer', true) is distinct from 'on' or pg_trigger_depth() > 0)
  execute function user_threads_unmarked_by_older_writer();
