-- 20261126042544_model_requests.up.sql — the model requests each project's hosts report, kept for
-- the provider's two signals of admission by signals (its headroom and its refusals), the token
-- limit each minute of them is read against, and the turns of each claim, which say where a host
-- that reports no requests left the record incomplete.
--
-- The headroom (signals.ProviderAt) is a minute's uncached input tokens over the project's own
-- limit in force in it, and the refusals (signals.RefusalsAt) the refusals of the hour before a
-- moment, by the project's own limit (an unknown one among them) and by the provider. Each host of
-- a session of the project (the Oh My Pi plugin, the Claude Code bridge) reports each request soon
-- after it ends (POST /legion/v1/claims/model-requests, intake.ModelRequests), and admission keeps
-- it in the fact's own transaction, before that fact's pass (admit's keepSignalFacts,
-- record.Store.KeepModelRequests):
--
--   - model_requests is one row per request: the claim and launch generation whose host reported
--     it and the host's own id of it, which name it once (a request two reports carry is one row),
--     the session it ran in, the model, when it ended (which places it in its own UTC minute), its
--     uncached input tokens (null where the host did not learn them), whether its model is one whose
--     tokens count against the limit (signal_token_model_pattern, as the daemon was configured when
--     it kept the row, so a changed selector reads no row again), and for a refused request who
--     refused it ('own', 'provider' or 'unknown', kept apart; the signal counts 'unknown' as own).
--     No prompt, answer or text of an error is kept.
--   - model_request_minutes is one row per project and UTC minute that holds a kept request: the
--     sums of that minute's requests as ProviderMinutes reads them, the uncached input tokens of
--     the counted ones, the refusals by the project's own limit (an unknown one among them) and by
--     the provider, and whether one of its requests came in a batch that said its host does not
--     report the 429s its transport retries (true until a later migration keeps that word, as no
--     batch said it then). Each request a write keeps adds to its minute's row in the same
--     statement, and a request kept already adds nothing, so a read of an hour reads 60 rows rather
--     than every request of the hour; the same prune deletes both.
--   - signal_token_limits is one row per project and moment the daemon's configured token limit
--     (signal_token_limit_per_minute) took a value: each boot writes one where the limit differs
--     from the latest row's, null for none. A minute is read against the row in force at its start,
--     so a changed limit never reads an earlier minute again.
--   - claim_turns is one row per turn of a claim, from the claim's own turn edges (claims'
--     last_turn_start and last_turn_end, which every write of the claim carries,
--     store.Store.PutClaim): the project token, the claim, its generation, the plugin contract its
--     host registered with, when the turn began and when it ended (null while it runs). A host of
--     a contract before the one that reports model requests spends tokens no report names, so each
--     minute such a turn overlaps reads as not recorded, never as zero (record.Postgres's
--     ProviderMinutes); the claim's contract is kept on the row, and the read compares it.
--
-- Every time is a host's, the machines' clock's or the daemon's clock's, never a column default.
-- This migration copies into claim_turns the turn each claim is in as it runs (state working, its
-- last turn's start after its last turn's end, as store's keepClaimTurn opens a turn), since its
-- start was written before the table existed and its end will be written after.
--
-- The migration adds four tables and changes none that an earlier release reads, so a daemon of
-- the release before runs over the migrated database, as a rollback leaves it: it keeps nothing
-- here and moves no row.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon that shares the database stopped and before
-- the release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. Dropping the
-- four tables, deleting the provider's row of signal_inputs and deleting this version row lets the
-- roll-forward apply this migration anew and record a fresh kept_since at its first boot, from
-- which the provider's signals count again, rather than read the requests of a release that kept
-- none since as a quiet hour.
--
-- begin;
-- drop table if exists model_requests;
-- drop table if exists model_request_minutes;
-- drop table if exists signal_token_limits;
-- drop table if exists claim_turns;
-- delete from signal_inputs where input = 'provider';
-- delete from schema_version where version = 20261126042544;
-- commit;
create table model_requests (
    project    text        not null check (project <> ''),
    claim      text        not null check (claim <> ''),
    generation bigint      not null check (generation >= 1),
    id         text        not null check (id <> ''),
    session    text        not null check (session <> ''),
    model      text        not null check (model <> ''),
    ended_at   timestamptz not null,
    uncached   bigint      check (uncached >= 0),
    counted    boolean     not null,
    refusal    text        check (refusal in ('own', 'provider', 'unknown')),
    primary key (project, claim, generation, id)
);
create index model_requests_by_time on model_requests (project, ended_at);
create table model_request_minutes (
    project    text        not null check (project <> ''),
    minute     timestamptz not null,
    uncached   bigint      not null check (uncached >= 0),
    own        bigint      not null check (own >= 0),
    provider   bigint      not null check (provider >= 0),
    unreported boolean     not null default true,
    primary key (project, minute)
);
create table signal_token_limits (
    project     text        not null check (project <> ''),
    since       timestamptz not null,
    token_limit bigint      check (token_limit >= 1),
    primary key (project, since)
);
create table claim_turns (
    project         text        not null check (project <> ''),
    claim           text        not null check (claim <> ''),
    generation      bigint      not null check (generation >= 0),
    plugin_contract integer     not null,
    started_at      timestamptz not null,
    ended_at        timestamptz,
    primary key (claim, started_at)
);
create index claim_turns_open on claim_turns (claim) where ended_at is null;
create index claim_turns_by_time on claim_turns (project, started_at);
insert into claim_turns (project, claim, generation, plugin_contract, started_at)
select project, token, generation, plugin_contract, last_turn_start from claims
where state = 'working' and last_turn_start is not null and (last_turn_end is null or last_turn_start > last_turn_end);
