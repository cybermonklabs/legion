-- 20261127073000_kept_delivery_expiries.up.sql
--
-- The copies of Dispatch's delivery of a post that the listener kept for a role's first holder
-- and told Dispatch had ended with no session taking them (bus.KeptCopyExpiry, read by the outbox
-- in internal/dispatch/outbox/role_expiry_notices.go): one row per copy, post and role, written
-- when the outbox reads the listener's notice of that copy, whether or not any attempt is queued
-- under the copy's envelope yet.
--
-- The attempt a copy carries is recorded queued, with the copy's envelope id, when the API's send
-- reports (internal/dispatch/api, settleDeliveryAttempt), and a listener's notice can be read
-- before that: the copy's hour is the listener's, and the send's report waits on the API's own
-- transaction. The notice then finds the attempt still pending and settles nothing, so the API
-- reads this table, in the transaction that would write queued and under the lock on the same
-- owner the outbox's record takes, and settles the attempt failed instead. Both orders end with
-- the attempt failed, with its envelope cleared.
--
-- envelope_id is the copy's envelope id, which every attempt of the post that the copy carried
-- records. role is the role the copy was kept on, which the notice names and the outbox checks
-- against the copy it reads back from that role's kept lane: the attempt's own target is
-- 'role:' || role, so a notice stored for one role is never the record of another role's attempt,
-- and the key holds the role beside the envelope, so a row of one role cannot hold a row of
-- another's back. post_kind and post_id name the comment or message the copy delivers, and are
-- part of the key for the same reason. forward_unconfirmed is whether the listener began to
-- forward the copy and never confirmed that forward, so the role's holder may already have the
-- message, which the settled attempt's error says (model.KeptDeliveryExpiredError). A second
-- notice of the copy keeps a true value.
--
-- recorded_at is when the outbox recorded the notice, for an operator and for a later pruning of
-- rows whose copies' attempts are long settled.
--
-- A new table: nothing existing is read, locked or rewritten here, and the release before this one
-- neither reads nor writes it, which is what a rolling update requires of a migration
-- (cmd/dispatch/README.md, "Database migrations").
create table kept_delivery_expiries (
    envelope_id text not null,
    role text not null,
    post_kind text not null check (post_kind in ('comment', 'message')),
    post_id uuid not null,
    forward_unconfirmed boolean not null default false,
    recorded_at timestamptz not null default now(),
    primary key (envelope_id, role, post_kind, post_id)
);
