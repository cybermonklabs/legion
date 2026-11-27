-- 20261122100123_events_inbox_owed.up.sql
--
-- The events of the writes an older build makes that change no row this release marks:
--   events_inbox_owed   one row for each event a build before this release appends of a write
--                       that moves who an open ask waits on or edits or deletes a comment or an
--                       issue message, its id, which the backfill and the block-index tick's
--                       catch-up read and delete once they have stated what the write owes the
--                       store (graphcensus's inboxEventsPass, writeQueuedInboxEvents).
--
-- This release's writers state, in the write's own transaction, what each of these writes owes
-- the node and tuple store. Three writes move who an ask waits on (the spec's R2) - patchIssue's
-- change of an issue's assignee (issue.updated, whose payload names assignee among the fields it
-- changed), putAskAskedOf's hand-on (ask.asked_of) and putProjectDefaultAssignee's move of a
-- project's default (settings.project_default_assignee.updated) - and so does the restore of an ask
-- that was not open, by an issue's reopen or by a settlement (ask.opened): each appends the new
-- person's delivered_to and participates_in and retracts the old person's
-- (inboxwrite.QueueMoveWaiting). An edit of a comment (comment.edited) or of an issue message
-- (message.edited) states the mentions tuple to each person its new text names and their
-- participates_in in the item's thread (R5, R4); that edit, and a delete of either
-- (comment.deleted, message.deleted), which leaves the post no text, retract the mention of each
-- identity the text no longer names. A build before this release writes the issue's, the ask's,
-- the project's, the comment's or the message's row and nothing of the store, and none of those
-- rows' markers moves: asks.tuples_stored_at is the moment of the ask's create and answer the
-- store holds, comments.tuples_stored marks a comment's pointers and messages.node_kind a
-- message's, and no edit or delete moves either. So no pass over a marker reads such a write
-- again, and a mention of a person is no row at all (refs.TargetOf). The trigger below queues the
-- write's event instead, in the statement that appends it. The backfill and the block-index tick's
-- catch-up take the queue one batch at a time (graphcensus.Backfill, graphcensus.CatchUpOwedRows),
-- each event once, and the tick owes its catch-up while the queue holds a row
-- (graphcensus.EventsOweObjectPass). A write of this kind that build made before this migration
-- touched no ask, comment or message the store holds the tuples of: only this release states them,
-- after its migrations, and the backfill's asks, comment pointer and messages passes state each row
-- that build wrote as it stands when they run.
--
-- The trigger's WHEN is the marker triggers' own (20261116150000, 20261116160000, 20261116170000,
-- 20261122100122) with the event types above: every connection of this release's pool carries
-- dispatch.store_marker_writer `on` (store.StoreMarkerWriterSetting), so an event this release
-- appends never calls the function, and the WHEN test is all its writes pay; the function keeps
-- the same test, so a trigger that calls it without the WHEN still queues only an older build's
-- event. It queues an issue.updated only where the payload's changed names assignee, the one change
-- of an issue that moves its asks; a repeat of an event already queued is kept once. It writes only
-- the queue, which no other writer takes a lock of before an events row, so it adds to that build's
-- write one step after the events row it appends, and the catch-up takes the queue's rows after the
-- owners of the asks, comments and messages they name, as that build's writer takes those owners
-- before it appends.
--
-- The table is new and holds no row. `create trigger` takes SHARE ROW EXCLUSIVE on events for its
-- catalogue write and holds it until the migration commits, at once, since no other statement of
-- the migration waits on anything: the lock blocks every writer of events, which is every write
-- Dispatch makes, and no reader, for that write alone. It must first wait for every transaction
-- that holds a write lock on events, and every event append queued behind it waits as long. Half a
-- second bounds that wait, as 20261122100122 bounds its own: past it the migration fails with
-- 55P03 having applied nothing, the appends go on, and the runner applies it again
-- (pgmigrate.LockTimeoutAttempts). The migration takes no lock of any other table, so it closes no
-- cycle with a write of the release still serving. The release before this one never reads the
-- table; its writes of these kinds write one more row through the trigger, and rolling back past
-- this migration is an image rollback alone, after which the queue fills with the writes of the
-- build rolled back to until this release serves again and drains it.
set local lock_timeout = '500ms';

-- kept for one release: events-inbox-owed
create table events_inbox_owed (
  event_id bigint primary key
);

-- kept for one release: events-inbox-owed
create function events_inbox_owed_by_older_writer() returns trigger language plpgsql as $$
begin
  if current_setting('dispatch.store_marker_writer', true) is distinct from 'on'
     and (new.type in ('ask.asked_of', 'ask.opened', 'settings.project_default_assignee.updated', 'comment.edited', 'comment.deleted',
                       'message.edited', 'message.deleted')
          or (new.type = 'issue.updated' and coalesce(new.payload->'changed', '[]'::jsonb) @> '["assignee"]'::jsonb)) then
    insert into events_inbox_owed (event_id) values (new.id) on conflict do nothing;
  end if;
  return null;
end;
$$;

-- kept for one release: events-inbox-owed
create trigger events_inbox_owed after insert on events
  for each row
  when (current_setting('dispatch.store_marker_writer', true) is distinct from 'on'
        and new.type in ('issue.updated', 'ask.asked_of', 'ask.opened', 'settings.project_default_assignee.updated', 'comment.edited',
                         'comment.deleted', 'message.edited', 'message.deleted'))
  execute function events_inbox_owed_by_older_writer();
