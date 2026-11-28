-- 20261116094741_spec_check_results.up.sql
--
-- One row per result a CI job wrote for a check of the spec files of its commit
-- (POST /api/v1/projects/{key}/results, with a results token the secrets broker minted for the
-- job's run). The row holds who and what Dispatch read, never what the caller said of itself:
--   seq               Dispatch's record order, which the read orders by.
--   project_key       the project whose results the token writes.
--   repository        the run's repository, owner/name, as the token names it.
--   check_id          the check's id as the files hold it.
--   commit_sha        the 40-hex commit of the run's token, which the result is for.
--   procedure_digest  the check's digest as the files at that commit hold it.
--   job, leg          the name of the run's check run, cut at its last " (": the job, and its
--                     matrix values ('' with no matrix).
--   check_run_id, run_id, attempt
--                     the run's check run, workflow run and attempt, from the token.
--   verdict           'pass', 'fail' or 'could_not_run'.
--   attestation       the in-toto statement the read answers, whole.
--   session           the session of the token, its workflow's own.
--   recorded_at       when the row was written.
-- Nothing updates or deletes a row: a result is a record of one run, and another run, another
-- attempt or another leg is another row. One writer inserts them (insertSpecCheckResult). The
-- table has no foreign key, so no delete elsewhere removes a row and no write here locks another
-- table; the route has already found the project the token names.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0, and rolling back past it is an image rollback alone.
create table spec_check_results (
    id uuid primary key default gen_random_uuid(),
    seq bigint generated always as identity unique,
    project_key text not null,
    repository text not null,
    check_id text not null,
    commit_sha text not null check (commit_sha ~ '^[0-9a-f]{40}$'),
    procedure_digest text not null,
    job text not null,
    leg text not null,
    check_run_id bigint not null check (check_run_id > 0),
    run_id bigint not null check (run_id > 0),
    attempt integer not null check (attempt > 0),
    verdict text not null check (verdict in ('pass', 'fail', 'could_not_run')),
    attestation jsonb not null,
    session text not null,
    recorded_at timestamptz not null default now()
);

-- The read: every result of one project at one commit, by check, in record order.
create index spec_check_results_commit on spec_check_results (project_key, commit_sha, check_id);
