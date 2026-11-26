-- 20261126103753_code_survival.up.sql
--
-- The results of the code survival and post-merge rework measurement (dispatch://INFRA-3206, R3,
-- R8, R9): what a run of internal/dispatch/codesurvival keeps of each patch it read from GitHub,
-- which is counts, SHAs, branch names, logins and issue keys, and never a patch, a file's text or
-- a list of a branch's commits. Five new tables:
--
-- code_repos holds one row per repository whose default branch the run reads:
--   repo              the repository, in lower case, as projects.default_repo is checked.
--   branch            the default branch the run read it as.
--   start_day         the first day the run measures; no day before it is (R4).
--   state             readable, or unreadable where the App cannot read the repository, which
--                     state_reason then says and which the next daily pass tries again.
--   checked_on        the UTC day of the last daily pass that finished its read.
--   last_measured_on  the UTC day a run last stored a result.
--   retry_at          when a run that stopped at the budget may start again.
--
-- code_changes holds one row per measured change, the commit a pull request landed as: its day
-- (the UTC date of its committer time), its pull request, head branch, author login and issue,
-- the lines it added, the lines of those still there at its 30-day mark (alive) and the lines that
-- commits which merged no pull request removed before that mark (removed_by_non_pr). Its other
-- removals are rows of code_pairs, so a save of a project's code groups can regroup every change
-- and pair with no patch read.
--
-- code_pairs holds one row per pair of a measured change (the adder) and a later change (the
-- remover) that removed some of its lines before its mark: the remover's SHA, pull request, head
-- branch, author login and issue, the lines it removed and the days between the two commits. A
-- pair goes with its adder, by its key: the key (repo, adder_sha) references the adder's
-- (repo, sha) on delete cascade, so the rows of a change a run replaces take their pairs with
-- them, and on update cascade, so the boot's rename pass (store.RewriteRenamedRepositories), which
-- writes a renamed repository's new name in code_changes.repo, carries each change's pairs to the
-- new name in the same statement.
--
-- code_days holds one row per day a run replayed, whether it measured the day or the day lay in
-- the 30 days after one: the count of its first-parent commits and the sha256 of their SHAs in
-- chain order, which each run compares with the chain to find a force-push (R8). state says what
-- the day is:
--   replayed  a fingerprint and nothing else; the day still waits to be measured.
--   measured  a run stored the day's changes and pairs (measured_at, tried_at).
--   marked    a measured day that a later run found changed, or whose 30 days hold a changed
--             day (marked_at); it reads as no data until a run measures it again.
--   blocked   a commit whose patch neither route gave whole blocks the day (R9): blocked_sha and
--             blocked_files name it, blocked_reason may say why, tried_at says when. The SHA is
--             kept only while the day is blocked, and the row that measures the day replaces it.
-- Each state's checks name the columns it holds and the columns it leaves null, so no row says
-- two things.
--
-- code_projects holds one row per project whose day rows the run or the regroup built: the sha256
-- of the code groups they stand built under (groups_digest) and when (built_at), both null
-- until a build. The builders lock the row, so a run and a regroup never build one project's rows
-- together.
--
-- A SHA is stored only for a change, for the remover of a stored pair, and for a blocked day's
-- blocking commit. No column holds patch text: the bytea columns hold 32-byte digests, which
-- their checks hold them to.
--
-- Five new tables: nothing existing is read, locked or rewritten except projects, whose rows
-- code_projects' foreign key will check; creating that key takes SHARE ROW EXCLUSIVE on projects
-- for the statement, bounded by the runner's lock_timeout (pgmigrate.LockTimeout, 5 s). The
-- release before this one neither reads nor writes these tables, so the two builds roll together,
-- and the census answers 0 since every table is new. The identifier is the UTC moment this file
-- was created, as `migrationid` requires of every migration past the numbered series.
create table code_repos (
    repo             text primary key check (repo <> '' and repo = lower(repo)),
    branch           text check (branch <> ''),
    start_day        date,
    state            text not null check (state in ('readable', 'unreadable')),
    state_reason     text check (state_reason <> ''),
    checked_on       date,
    last_measured_on date,
    retry_at         timestamptz,
    constraint code_repos_reason_when_unreadable check ((state = 'unreadable') = (state_reason is not null))
);

create table code_changes (
    repo              text not null check (repo <> '' and repo = lower(repo)),
    sha               text not null check (sha ~ '^[0-9a-f]{40}$'),
    day               date not null,
    committed_at      timestamptz not null,
    pr_number         integer not null check (pr_number > 0),
    head_ref          text not null,
    author            text not null,
    issue_key         text check (issue_key <> ''),
    added             integer not null check (added >= 0),
    alive             integer not null check (alive >= 0),
    removed_by_non_pr integer not null check (removed_by_non_pr >= 0),
    primary key (repo, sha),
    constraint code_changes_day_is_the_commit_day check (day = (committed_at at time zone 'UTC')::date),
    constraint code_changes_lines_fit_in_added check (alive + removed_by_non_pr <= added)
);

create index code_changes_repo_day on code_changes (repo, day);

create table code_pairs (
    repo              text not null,
    adder_sha         text not null,
    remover_sha       text not null check (remover_sha ~ '^[0-9a-f]{40}$'),
    remover_pr        integer not null check (remover_pr > 0),
    remover_head_ref  text not null,
    remover_author    text not null,
    remover_issue_key text check (remover_issue_key <> ''),
    lines             integer not null check (lines > 0),
    days_between      double precision not null,
    primary key (repo, adder_sha, remover_sha),
    foreign key (repo, adder_sha) references code_changes (repo, sha) on update cascade on delete cascade
);

create table code_days (
    repo           text not null check (repo <> '' and repo = lower(repo)),
    day            date not null,
    commits        integer not null check (commits >= 0),
    digest         bytea not null check (octet_length(digest) = 32),
    state          text not null check (state in ('replayed', 'measured', 'marked', 'blocked')),
    measured_at    timestamptz,
    marked_at      timestamptz,
    blocked_sha    text check (blocked_sha ~ '^[0-9a-f]{40}$'),
    blocked_files  integer check (blocked_files > 0),
    blocked_reason text check (blocked_reason <> ''),
    tried_at       timestamptz,
    primary key (repo, day),
    constraint code_days_replayed_holds_a_fingerprint_only check (state <> 'replayed' or (
        measured_at is null and marked_at is null and blocked_sha is null and blocked_files is null
        and blocked_reason is null and tried_at is null)),
    constraint code_days_measured_columns check (state <> 'measured' or (
        measured_at is not null and tried_at is not null and marked_at is null
        and blocked_sha is null and blocked_files is null and blocked_reason is null)),
    constraint code_days_marked_columns check (state <> 'marked' or (
        measured_at is not null and tried_at is not null and marked_at is not null
        and blocked_sha is null and blocked_files is null and blocked_reason is null)),
    constraint code_days_blocked_columns check (state <> 'blocked' or (
        blocked_sha is not null and blocked_files is not null and tried_at is not null
        and measured_at is null and marked_at is null))
);

create table code_projects (
    project_key   text primary key references projects (key) on delete cascade,
    groups_digest bytea check (octet_length(groups_digest) = 32),
    built_at      timestamptz,
    constraint code_projects_digest_and_time_together check ((groups_digest is null) = (built_at is null))
);
