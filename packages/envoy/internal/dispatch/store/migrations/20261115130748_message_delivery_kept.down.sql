-- Rolls 20261115130748 back by hand, once no running build writes `kept` (the image is rolled
-- back first). The narrow check refuses a kept attempt, so each one becomes the failure the
-- release before this one records for it, "no live session <session>", with a note that its copy
-- was kept: that copy stays on the session's own subject and still reaches the session when it
-- registers again. The person then reads a failed attempt they may retry; a same-mode retry repeats
-- the kept copy's dedupe key, so the stream drops it as a repeat inside its duplicate window
-- rather than handing the session the message twice. message_deliveries_session_kept then holds no
-- row and the release before this one never reads it, so it goes; 0147's index was never changed.
update message_deliveries
set state = 'failed',
    error = 'no live session ' || session_id || ' (a copy was kept for the session before a rollback)'
where state = 'kept';
alter table message_deliveries drop constraint message_deliveries_state_check;
alter table message_deliveries add constraint message_deliveries_state_check
  check (state in ('pending', 'sent', 'failed')) not valid;
drop index message_deliveries_session_kept;
