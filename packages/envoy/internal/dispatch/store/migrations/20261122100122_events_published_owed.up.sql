-- 20261122100122_events_published_owed.up.sql
--
-- The events whose published destinations an older build's outbox recorded:
--   events_published_owed   one row for each event about a comment or a message whose
--                           published_destinations a build before this release's outbox grew,
--                           its id, which the block-index tick's catch-up reads and deletes once it
--                           has stated the delivered_to tuples those destinations owe the store
--                           (graphcensus's publishedDeliveriesQueued, writePublishedDeliveries).
--
-- This release's outbox appends, in the statement that adds the topics it published an event to
-- the event's published_destinations, one delivered_to tuple from the event's comment or message to
-- the session or the role each topic names (outbox's recordBatch and deliveredGraph), so the
-- session and role topics of that array are the far ends of those tuples (the census's
-- published_destinations kind). The outbox of the store's own release states them for a comment's
-- event alone, and a build before it states none. A process of such a build that holds the
-- publisher hold during a roll records its events with no tuple, and that record appends no event
-- and lands after the event commits, so the block-index tick, which reads each event once after
-- its cursor, has read the event already when the record lands, and no read of the log tells it to
-- look again. The trigger below queues the event instead, in the statement that records it, and the
-- tick's catch-up takes the queue one batch a tick (graphcensus.CatchUpOwedRows); the backfill
-- states every such event recorded before it ran from the log (graphcensus's
-- publishedDeliveriesPass).
--
-- The trigger's WHEN is the marker triggers' own (20261116150000, 20261116160000, 20261116170000):
-- every connection of this release's pool carries dispatch.store_marker_writer `on`
-- (store.StoreMarkerWriterSetting), so this release's outbox, which states the tuples itself, never
-- calls the function, and the function keeps the same test, so a trigger that calls it without the
-- WHEN still queues only an older build's record. The function queues an event about a comment or a
-- message alone, the events this release's outbox states a delivery of (outbox's deliveredItem),
-- and only where the record grew the array; a repeat of an event already queued is kept once. It
-- writes only the queue, which no other writer takes a lock of before an events row, so it adds to
-- that build's record one step after the events rows it updates, as the tick's catch-up takes the
-- events rows of its batch before it deletes their queue rows.
--
-- The table is new and holds no row. `create trigger` takes SHARE ROW EXCLUSIVE on events for its
-- catalogue write and holds it until the migration commits, at once, since no other statement of
-- the migration waits on anything: the lock blocks every writer of events, which is every write
-- Dispatch makes, and no reader, for that write alone. It must first wait for every transaction
-- that holds a write lock on events, and every event append queued behind it waits as long. Half a
-- second bounds that wait, as 20261116150000 bounds its own: past it the migration fails with
-- 55P03 having applied nothing, the appends go on, and the runner applies it again
-- (pgmigrate.LockTimeoutAttempts). The migration takes no lock of any other table, so it closes no
-- cycle with a write of the release still serving. The release before this one never reads the
-- table; its outbox's record writes one more row through the trigger, and rolling back past this
-- migration is an image rollback alone, after which the queue fills with the records of the build
-- rolled back to until this release serves again and drains it.
set local lock_timeout = '500ms';

-- kept for one release: events-published-owed
create table events_published_owed (
  event_id bigint primary key
);

-- kept for one release: events-published-owed
create function events_published_owed_by_older_writer() returns trigger language plpgsql as $$
begin
  if current_setting('dispatch.store_marker_writer', true) is distinct from 'on'
     and (new.type like 'comment.%' or new.type like 'message.%')
     and new.published_destinations is distinct from old.published_destinations then
    insert into events_published_owed (event_id) values (new.id) on conflict do nothing;
  end if;
  return null;
end;
$$;

-- kept for one release: events-published-owed
create trigger events_published_owed after update of published_destinations on events
  for each row
  when (current_setting('dispatch.store_marker_writer', true) is distinct from 'on')
  execute function events_published_owed_by_older_writer();
