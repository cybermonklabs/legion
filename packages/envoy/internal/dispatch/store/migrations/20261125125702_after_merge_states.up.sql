-- 20261125125702_after_merge_states.up.sql
--
-- What the results read needs to say whether a result's commit holds the merge of the work, which
-- of two merges is the newer, and which deploy a check that reads a live system counts results of.
--
--   pull_request_heads.merge_commit_sha
--                      the commit that merged the pull request into its base branch, as the
--                      `pull_request` envelope or GitHub's answer for one pull request names it;
--                      '' where the row is not merged or no write has carried one yet. Before
--                      this column PullRequestHead.MergeCommitSHA only seeded branch_commits and
--                      was written to no column (delivery/pull_request_heads.go).
--   pull_request_heads.merged_at
--                      when GitHub merged it, so a read orders two merges by the time each one
--                      merged, not by event_at, the newest event's updated_at, which a later edit
--                      of an older pull request moves. Null where the row is not merged or no
--                      write has carried it yet.
--   pull_request_heads_merged_head_ref
--                      the merged rows of one repository by their head branch: the read that
--                      follows a branch's landing to the default branch asks which merged pull
--                      request had that branch as its head (delivery.LandingOf).
--   commit_holds       GitHub's answer whether one commit's history holds one merge commit, keyed
--                      by the repository of the commit, lowercased as branch_commits keys it. Two
--                      commits never change, so a no is kept for good as a yes is. A read that
--                      failed is stored nowhere (delivery.Branches.CommitHolds).
--   deploys            the record of deploys: the commit of a repository that a program runs, and
--                      since when. program is the deployed program's name; the current deploy of
--                      a program is its row of the latest deployed_at, then the higher id. source
--                      is 'delivery_run', a row the delivery timeline writes for one production
--                      run (run_id, run_attempt; one row per run), or 'route', a row a deploy job
--                      posts. recorded_by is the actor that wrote it, as check_results.actor is.
--   delivery_runs.run_attempt
--                      GitHub's actual attempt, null for existing rows until a later GitHub read
--                      supplies it. Intake and reconcile preserve unknown rather than invent one.
--   deploys.run_completed_at
--                      the source run completion this writer already observed. It advances when a
--                      same-build rerun leaves deployed_at unchanged, preventing a later replay.
--   check_results.check_subject, check_results.check_program
--                      the check block's `subject` and `program` attributes as the record route
--                      read them when it stored the result, '' where the check named none. They
--                      decide no answer: they bound which open issues a recorded deploy is
--                      announced on. check_results_live holds the rows that name a subject.
--
-- How the merge columns fill. Any read of GitHub that writes a merged row writes them (headRow,
-- upsertHeadRow): the reconcile's facts line, which reads each row whose texts are not current,
-- a merged row stored before these columns among them, and a results read's refresh of a merged
-- row it must order and that holds no time (api's fillMergeTimes, RefreshMergedPullRequest). The
-- reconcile's own pass lists open rows alone (ListStalePullRequestHeads). A write that carries
-- neither column keeps both as stored.
-- No result recorded before this migration can name a subject or a program, since no build lets an
-- author write either yet, so no backfill is owed.
--
-- Adding a column with a constant default, or a nullable one, writes the catalog alone and
-- rewrites no row; each ALTER holds ACCESS EXCLUSIVE on its table for that write. Each create index
-- on an existing table holds SHARE on it while it reads it, inside the runner's one transaction:
-- pull_request_heads holds a row per pull request Dispatch has seen an event for, and check_results
-- a row per recorded result. commit_holds and deploys are new. The release before this one neither reads nor
-- writes any of it, and names its columns in each insert (upsertHeadRow, insertCheckResult), so the
-- defaults serve every row it writes while it still serves (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0.
--
-- The way back is an image rollback alone. By hand, with no process of this release serving, the
-- block below, which is 20261125125702_after_merge_states.down.sql, also takes the schema back: it
-- loses every stored hold, deploy, merge commit, merge time and mark, and its last statement makes
-- the next boot of this release apply this migration again, which then reads each hold from GitHub
-- again, refreshes each merge it must order, and holds no deploy until the next one is recorded.
--
-- drop index check_results_live;
-- alter table check_results drop column check_program, drop column check_subject;
-- drop table deploys;
-- drop table commit_holds;
-- drop index pull_request_heads_merged_head_ref;
-- alter table pull_request_heads drop column merged_at, drop column merge_commit_sha;
-- alter table delivery_runs drop column run_attempt;
-- delete from schema_migrations where version = 20261125125702;
alter table pull_request_heads add column merge_commit_sha text not null default '';
alter table pull_request_heads add column merged_at timestamptz;
alter table delivery_runs add column run_attempt integer;
create index pull_request_heads_merged_head_ref on pull_request_heads (lower(repo), head_ref)
  where state = 'merged';

create table commit_holds (
    repo text not null,
    commit_sha text not null,
    merge_sha text not null,
    holds boolean not null,
    checked_at timestamptz not null,
    primary key (repo, commit_sha, merge_sha)
);

create table deploys (
    id bigserial primary key,
    repo text not null,
    program text not null check (program ~ '^[a-z0-9-]{1,64}$'),
    commit_sha text not null check (commit_sha ~ '^[0-9a-f]{40}$'),
    deployed_at timestamptz not null,
    source text not null check (source in ('delivery_run', 'route')),
    run_id bigint,
    run_attempt integer,
    -- Last source completion observed, which moves even when the same build's deployed_at does not.
    run_completed_at timestamptz,
    url text not null default '',
    recorded_at timestamptz not null default now(),
    recorded_by jsonb not null
);
-- The current deploy of each program of a repository: its newest row first.
create index deploys_newest on deploys (repo, program, deployed_at desc, id desc);
-- One row per production run of the delivery timeline, which a re-run of that run updates.
create unique index deploys_run on deploys (source, run_id) where run_id is not null;

alter table check_results add column check_subject text not null default '';
alter table check_results add column check_program text not null default '';
create index check_results_live on check_results (check_subject) where check_subject <> '';
