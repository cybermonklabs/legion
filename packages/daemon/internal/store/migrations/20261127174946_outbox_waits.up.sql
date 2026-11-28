-- 20261127174946_outbox_waits.up.sql — the wait of an outbox row that waits for its role, or for an
-- event the daemon records itself, kept on the row (dispatch://INFRA-4131).
--
-- A row of the kinds that publish a message to one role (internal/record's
-- OutboxKind.WaitsForRole), and a row that waits for an event of the daemon's own, keep their wait
-- in seven columns, so that a restart of the daemon keeps it too (internal/record's OutboxWait):
--
-- * wait_since is the moment the row's current wait started, and null while it waits for nothing.
--   The wait is logged once at its start, once at 30 minutes and once at its end by it.
-- * wait_logged is whether the wait's 30-minute line was written.
-- * wait_step is the step of the wait's backoff, which doubles from one second to five minutes
--   (record.RoleWaitBackoff).
-- * wait_ready is the ready of the role's claim (supervise.Claim.LastReady) that wait_step counts
--   from, and null for none.
-- * wait_launch is the launch of the role's claim (supervise.Claim.Generation) the row last
--   published to, and 0 for none.
-- * wait_for is the key of what the row waits for (record.WaitForClaim and the other builders of
--   a key), and the empty string for nothing.
-- * wait_woken is whether an event the row waits for came while a lane held the row, so that the
--   row's retry makes it due at once rather than at its backoff.
--
-- outbox_wait_for indexes the rows that wait for something by their key, which is how a write that
-- records an event finds the rows it makes due (record.Store.ExpediteWaits). A row that waits for
-- nothing, which is most rows, is left out of it.
--
-- Every row the migration finds waits for nothing, and the seven columns take their defaults on
-- it. An earlier release names its columns in every read and write of the outbox, so its daemon
-- runs over the migrated database and ignores the seven: its inserts leave them to their defaults,
-- and its retry of a row leaves them as this release last wrote them, which this release goes on
-- from when it is rolled forward again. So no way back is needed, and the migration carries none:
-- a rollback leaves the columns and the index standing.

alter table outbox
  add column wait_since timestamptz,
  add column wait_logged boolean not null default false,
  add column wait_step integer not null default 0,
  add column wait_ready timestamptz,
  add column wait_launch bigint not null default 0,
  add column wait_for text not null default '',
  add column wait_woken boolean not null default false;

create index outbox_wait_for on outbox (wait_for) where wait_for <> '';
