-- 20261116213930_ci_check_results.up.sql
--
-- Two tables of what a CI job recorded: ci_check_results, its results, and
-- ci_check_result_retractions, the retractions of them.
--
-- ci_check_results holds one row per result a CI job recorded against a check its ci_check names
-- (POST /api/v1/issues/{key}/results under a results token the secrets broker minted for the
-- job's run). The row holds what Dispatch read from the token, from GitHub and from the check,
-- never what the caller said of itself:
--   issue_key          the issue the result was recorded on, the issue whose branch the job pushed.
--   check_issue_key    the issue whose Verification document holds the check, and the issue the
--                      result.recorded event is appended on.
--   check_artifact_id  that Verification document.
--   check_block_id     the check block's id within it.
--   check_version      document_blocks.changed_version of that block when the result was
--                      recorded: the document version whose words the result proves.
--   check_sha256       document_blocks.text_sha256 of that block at the same moment, which a read
--                      compares with the block's digest now.
--   run                the name of the run's check run, which is the check's ci_check.
--   surface, negative_control
--                      the check's own, as the block held them.
--   observed           what the job observed, kept by the rule of the project's results route.
--   commit_sha         the 40-hex commit of the run's token, which the job tested.
--   branch             the ref of the run without refs/heads/: the branch whose push it ran on.
--   verdict            'pass' or 'fail'.
--   attestation        the in-toto statement of the run, which the read answers whole.
--   session, actor     the session of the token, its workflow's own, and the actor naming it and
--                      no person.
--   event_seq          the seq of the result.recorded event on the check's issue.
--   commit_repo        the repository other than the check's issue's that the record route took
--                      commit_sha to be of, as check_results.commit_repo holds it for a role's
--                      row (api/results_linked.go, refuseUnknownCommit), and null for every other.
-- Nothing updates or deletes a row: a result is a record of one run, and another run or another
-- attempt is another row. One writer inserts them (insertCIJobCheckResult), and a retraction of
-- one is a row of ci_check_result_retractions, below. A row goes only with its issue or its
-- document, by the cascade check_results takes too.
--
-- A table of its own rather than more rows of check_results, which holds what a role recorded:
-- the release before this one reads every check_results row against a check block as a reason
-- that the check is no check CI proves, and its settlement then deletes the block's ci_check from
-- the live document. It reads no row of this table, so no CI job's result changes a check block
-- under any build.
--
-- New tables: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes either, so they roll beside this one (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0, and rolling back past it is an image rollback alone: the
-- release before reads none of their rows, so a result a job recorded changes nothing it serves.
create table ci_check_results (
    id uuid primary key default gen_random_uuid(),
    issue_key text not null references issues (key) on delete cascade,
    check_issue_key text not null references issues (key) on delete cascade,
    check_artifact_id uuid not null references artifacts (id) on delete cascade,
    check_block_id text not null,
    check_version integer not null,
    check_sha256 text not null,
    run text not null check (run <> ''),
    surface text not null default '',
    negative_control text not null default '',
    observed text not null default '',
    commit_sha text not null check (commit_sha ~ '^[0-9a-f]{40}$'),
    branch text not null check (branch <> ''),
    verdict text not null check (verdict in ('pass', 'fail')),
    attestation jsonb not null,
    session text not null,
    actor jsonb not null,
    created_at timestamptz not null default now(),
    event_seq bigint,
    commit_repo text
);

-- The results read: every result of one Verification document's checks, newest first.
create index ci_check_results_check on ci_check_results (check_artifact_id, check_block_id, created_at desc);
-- Every result an issue recorded, which the delete of an issue cascades through.
create index ci_check_results_issue on ci_check_results (issue_key);

-- ci_check_result_retractions holds one row per retracted result of ci_check_results: the record
-- that says such a result is wrong (POST /api/v1/results/{id}/retract), which takes a CI job's
-- result from the same three callers as a role's, in the shape check_result_retractions gives a
-- role's. Nothing in ci_check_results is updated or deleted, so the result and its retraction
-- stay readable together, each with its own reason and actor.
--   result_id    the retracted result, and this table's key. One retraction per result: a
--                second retraction of one result is refused by this primary key and by the
--                route before it.
--   reason       why the result is wrong, which the person or session who retracted it wrote.
--   session      the session that retracted it ('' for a person).
--   actor        the actor that retracted it, in the shape every other table stores an actor in.
--   created_at   when the retraction was written.
--   event_seq    the seq of the result.retracted event on the check's issue.
-- A table of its own rather than rows of check_result_retractions, whose result_id is a foreign
-- key into check_results: a row here goes with its result, by its own foreign key's cascade.
create table ci_check_result_retractions (
    result_id uuid primary key references ci_check_results (id) on delete cascade,
    reason text not null check (reason <> ''),
    session text not null default '',
    actor jsonb not null,
    created_at timestamptz not null default now(),
    event_seq bigint
);
