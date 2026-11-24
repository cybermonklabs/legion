-- 20261124134448_outbox_role_copy_expiries.up.sql
--
-- The copies the outbox kept on a role's kept lane whose hour ended with no session taking them
-- (internal/dispatch/outbox/role_expiry.go): one row per (event, role, keep_attempt), written when
-- the outbox reads the listener's expiry notice of that copy (bus.KeptCopyExpiry). keep_attempt is
-- the attempt the copy that ended was kept for, 1 for the batch's own keep; copy_sequence is where
-- the stream holds that copy on the role's kept lane, which is what the outbox reads back to keep
-- it again and what tells a second notice of the same copy from another copy's; dedupe_key,
-- kept_at and dropped_at are the notice's. A row whose keep_attempt is below the outbox's bound
-- (maxRoleKeeps) owes a keep of the next attempt, which the outbox's expiry pass makes and settles
-- the row by: outcome kept (the copy is on the lane again), forwarded (a session held the role by
-- then and was sent it) or skipped (the holder was the event's own actor); a row at the bound is
-- written settled with outcome undelivered, in the transaction that appends the event's
-- role.undelivered record, and so is a row whose copy the stream no longer holds. next_attempt_at
-- and attempt_count are the pass's claim and backoff, as comment_fallback_copies keeps them.
--
-- outbox_kept_expiry_cursor is its one row: the stream sequence of the last expiry notice the
-- outbox read, so a read goes on from the notice after it.
--
-- The partial index holds the unsettled rows alone, in the order the pass reads them: a pass with
-- nothing to keep reads an empty index, and a settled row leaves it.
--
-- Two new tables and an index: nothing existing is read, locked or rewritten here, and the release
-- before this one neither reads nor writes either table, which is what a rolling update requires
-- of a migration (cmd/dispatch/README.md, "Database migrations").
create table outbox_role_copy_expiries (
    event_id bigint not null,
    role text not null,
    keep_attempt integer not null check (keep_attempt >= 1),
    copy_sequence bigint not null,
    dedupe_key text not null,
    kept_at timestamptz not null,
    dropped_at timestamptz not null,
    received_at timestamptz not null default now(),
    next_attempt_at timestamptz,
    attempt_count integer not null default 0,
    settled_at timestamptz,
    outcome text check (outcome in ('kept', 'forwarded', 'skipped', 'undelivered')),
    primary key (event_id, role, keep_attempt),
    check ((settled_at is null) = (outcome is null))
);

create index outbox_role_copy_expiries_unsettled on outbox_role_copy_expiries (received_at) where settled_at is null;

create table outbox_kept_expiry_cursor (
    id boolean primary key default true check (id),
    sequence bigint not null
);
