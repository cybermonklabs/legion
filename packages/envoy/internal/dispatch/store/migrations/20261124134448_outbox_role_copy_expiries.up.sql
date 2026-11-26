-- 20261124134448_outbox_role_copy_expiries.up.sql
--
-- The copies the outbox kept on a role's kept lane whose hour ended with no session taking them
-- (internal/dispatch/outbox/role_expiry.go): one row per (event, role, keep_attempt), written when
-- the outbox reads the listener's expiry notice of that copy (bus.KeptCopyExpiry). keep_attempt is
-- the outbox's own count of the pair's expiries, one more than the pair's earlier rows (1 for the
-- first); the notice's own attempt decides nothing. copy_sequence is where the stream holds that
-- copy on the role's kept lane, which is what the outbox reads back to keep it again and what tells a
-- second notice of the same copy from another copy's; dedupe_key, kept_at and dropped_at are the
-- notice's. A row whose keep_attempt is below the outbox's bound (maxRoleKeeps) owes a keep of the
-- next attempt, which the outbox's expiry pass makes and settles the row by: outcome kept (the copy
-- is on the lane again), forwarded (a session held the role by then and was sent it) or skipped
-- (the holder was the event's own actor); a row at the bound is written settled with outcome
-- undelivered, in the transaction that appends the event's role.undelivered record. A row whose
-- copy cannot be kept again - the stream no longer holds it, or holds in its place a message no read can decode or another event's copy, or
-- NATS refused its keep or its forward for good - is settled carried while another copy of the
-- event still carries its delivery to the role, and undelivered, with that record, when none does.
-- next_attempt_at and attempt_count are the pass's claim and backoff, as comment_fallback_copies
-- keeps them.
--
-- outbox_kept_expiry_cursor is its one row: the stream sequence of the last expiry notice the
-- outbox read, so a read goes on from the notice after it, and the identity of the stream that
-- sequence counts in (bus.StreamIDMetadataKey), so a read of a stream created again since starts
-- from that stream's first notice rather than from a sequence of the stream before it.
--
-- outbox_role_undelivered_notes holds one row per event recorded undelivered to a role, in the
-- transaction that appends its role.undelivered record: the issue the message about it is posted
-- on (the event's own issue, or the issue of the event's document), the document for a document
-- event, what the message names of the event (its type, its keeps, why it ended, and the reference
-- of the post or document it is about), and window_opened_at, the start of the hour its message
-- covers: the window of the same issue and role that opened less than an hour before, or now when
-- none did. Once a window is an hour old the outbox posts one message on its issue naming every
-- event of the window (postUndeliveredNotes) and writes posted_at and message_id; next_attempt_at
-- and attempt_count are that post's backoff. The state is all here, so a window outlives the
-- process that opened it.
--
-- Each partial index holds the unsettled rows alone, in the order a pass reads them: a pass with
-- nothing to do reads an empty index, and a settled row leaves it.
--
-- Three new tables and their indexes: nothing existing is read, locked or rewritten here, and the
-- release before this one neither reads nor writes any of them, which is what a rolling update
-- requires of a migration (cmd/dispatch/README.md, "Database migrations").
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
    outcome text check (outcome in ('kept', 'forwarded', 'skipped', 'carried', 'undelivered')),
    primary key (event_id, role, keep_attempt),
    check ((settled_at is null) = (outcome is null))
);

create index outbox_role_copy_expiries_unsettled on outbox_role_copy_expiries (received_at) where settled_at is null;

create table outbox_kept_expiry_cursor (
    id boolean primary key default true check (id),
    sequence bigint not null,
    stream_id text not null
);

create table outbox_role_undelivered_notes (
    event_id bigint not null,
    role text not null,
    issue_key text not null,
    artifact_id uuid,
    event_type text not null,
    keeps integer not null check (keeps >= 1),
    reason text not null check (reason in ('expired', 'copy_gone')),
    item_ref text,
    window_opened_at timestamptz not null,
    noted_at timestamptz not null default now(),
    posted_at timestamptz,
    message_id uuid,
    next_attempt_at timestamptz,
    attempt_count integer not null default 0,
    primary key (event_id, role),
    check ((posted_at is null) = (message_id is null))
);

create index outbox_role_undelivered_notes_unposted on outbox_role_undelivered_notes (window_opened_at) where posted_at is null;

create index outbox_role_undelivered_notes_window on outbox_role_undelivered_notes (issue_key, role, window_opened_at);
