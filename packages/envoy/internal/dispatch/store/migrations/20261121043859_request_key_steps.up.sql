-- 20261121043859_request_key_steps.up.sql
--
-- INFRA-4731: each committed step's entry of a keyed status change (POST
-- /api/v1/issues/{key}/status-change), in the order the steps committed, beside its request key's
-- recovery point. The point's head stays on the key's row (request_keys.recovery): the ids of the
-- events the steps appended on the issue the call names, the ids of the asks its close withdrew,
-- and the mark that every step ran with the answer's issue. Each step that writes adds its row
-- here in its own transaction, in the statement that adds to the head, right after the step's
-- write phase (api's writeIssue, through docs.Ledger.AfterWrites), and every reader of the point
-- reads the rows back under its changed member (api's recoveryPointRead), so a repeat answers each
-- entry's issue as its step's write answered it rather than as it stands then. A row goes with its
-- key through the cascade: when the sweep deletes keys past their window (sweepRequestKeys), and
-- when a claim finds its key past its window (claimRequestKey). The release of a key that
-- committed nothing never removes one, since a key that holds step rows holds a point.
--
--   id                the order the steps committed in, which the readers aggregate by, and with
--                     the key the row's primary key.
--   login             whose key it is, as request_keys names it.
--   idempotency_key   the key, as request_keys names it.
--   entry             the step's entry as the route answers it: the issue's key, its status and
--                     parent after the step, its last_seq before the step, and the issue as the
--                     step's write answered it (api's statusChangeEntry).
--
-- A table of its own rather than a member of the head: each step adds one row, where rewriting a
-- growing jsonb value at every step costs the square of the steps. The primary key leads with the
-- key and ends with id, so it serves both of the table's reads with no index beside it: every
-- reader's aggregate of one key's rows in id order, and the cascade's delete of one key's rows.
--
-- A new table nothing before this release names. The foreign key holds request_keys in SHARE ROW
-- EXCLUSIVE for the short transaction of the migration, so a keyed request's claim waits behind it
-- while a read does not; otherwise nothing existing is read or rewritten. The release before this
-- one neither reads nor writes the table, so the two roll side by side (cmd/dispatch/README.md,
-- "Database migrations"), and the census answers 0 because the table holds no row at this schema.
-- Rolling back past it is an image rollback alone. The identifier is the UTC moment this file was
-- created, as every migration past the numbered series is named.
create table request_key_steps (
    id bigint generated always as identity,
    login text not null,
    idempotency_key text not null,
    entry jsonb not null,
    primary key (login, idempotency_key, id),
    foreign key (login, idempotency_key) references request_keys (login, idempotency_key) on delete cascade
);
