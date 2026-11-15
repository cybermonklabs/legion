-- 20261115130748_message_delivery_kept.up.sql
-- A person's targeted message to a session the Envoy listener holds no record of is kept rather
-- than failed (INFRA-3740). Dispatch publishes the envelope the listener's send route would have
-- stored onto the session's own subject (notifications.agent.<session>), where the session reads
-- it when it registers again, and records the attempt `kept` (keepForSession,
-- internal/dispatch/api/message_kept.go). A kept attempt is settled: the delivery sweep owes it
-- nothing, the session's reply moves it to `sent` as it moves any attempt, and the accept route
-- takes it as it takes a sent one.
--
-- The new list admits every state the old one did, so no stored row can fail the new check. It
-- is added `not valid`, as 20261102233625 adds its widened check: a validated add reads every row
-- of the table while it holds the ACCESS EXCLUSIVE lock, where `not valid` changes the catalog
-- alone and still refuses every row written or updated after it. Nothing validates it later;
-- there is nothing to validate.
--
-- The release before this one writes no `kept` row and reads one as a state it does not know:
-- its dashboard as sent, its accept route as a failed attempt, and its reply route moves it to
-- sent on a body as it does any attempt.
alter table message_deliveries drop constraint message_deliveries_state_check;
alter table message_deliveries add constraint message_deliveries_state_check
  check (state in ('pending', 'sent', 'failed', 'kept')) not valid;

-- A session's answers list (GET /api/v1/agents/{session_id}/answers, sessionanswers.List) holds a
-- kept message as it holds a sent one: a person's words Dispatch put on the session's own subject,
-- which the session reads when it comes back, and which nothing moves to `sent` unless the session
-- replies, so a list of sent deliveries alone would never show it. Its read of the session's
-- deliveries takes the sent ones and the kept ones as two arms, each an index-only scan of a
-- partial index whose predicate is that arm's own single equality, for the reason 0147 gives:
-- message_deliveries_session_created (0147) for `state = 'sent'`, unchanged, and
-- message_deliveries_session_kept, below, for `state = 'kept'`, with the same key and columns.
--
-- One index over both states was tried first. On PostgreSQL 18.6, in a table of 4,000 deliveries
-- of 20 sessions with a session_id index beside it, a partial index over
-- `state in ('sent', 'kept')`, read with the same `in` list, gave a bitmap heap scan in the custom
-- and the cached generic plan alike, and a sequential scan once bitmap scans were turned off: no
-- plain or index-only scan of it was built at all. Two arms of one equality each are the shape
-- 0147 already gives an index-only scan, and keeping 0147's index as it is also leaves the
-- release before this one, which reads `state = 'sent'`, its own scan while both builds serve.
--
-- A plain `create index`, as 0147's is: the runner applies the migration inside one transaction,
-- where `concurrently` cannot run. The build reads every row of the table, and the ACCESS
-- EXCLUSIVE lock the check above took is held until the migration commits, so reads of
-- message_deliveries wait for the build as writes do; the census holds the table to its size
-- limit. The index holds no row when it is built, since no attempt is kept before this migration.
create index message_deliveries_session_kept on message_deliveries (session_id, created_at desc)
  include (message_id)
  where state = 'kept';
