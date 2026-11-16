-- 20261107234701_branch_commits.up.sql
--
-- What Dispatch's GitHub App answered about one commit of one branch of one repository: is that
-- branch's history carrying it? A result for an after-merge check is current at a commit on the
-- branch the check's issue's pull request merged into or on the repository's default branch,
-- and one for a default-branch check only at a commit on the repository's default branch; the
-- answer is read when the results are read, never written by a push, so this is the cache that
-- keeps those reads off GitHub.
--   repo         the repository, lowercased ("acme/widgets"). It is the one key this table has,
--                because two spellings of a repository meet here: a merge seeds a row under
--                GitHub's own spelling, and the results read asks under the lowercased name the
--                repository-to-project mapping stores. Every writer and reader lowercases
--                (delivery/branch.go, readBranchCommit and writeBranchCommit).
--   sha          the 40-hex commit asked about.
--   branch       the branch it was asked about, as GitHub names it ("main", "infra-138"), not
--                lowercased: a git branch name is case-sensitive.
--   on_branch    GitHub's answer. A yes is kept for good: a rewrite of a branch does not undo
--                that the work reached it. A no is re-read after five minutes, since a commit
--                merges later. A merge writes one yes of its own, for its own merge commit on
--                its base branch, which GitHub's merge proves and nothing can prove later
--                (delivery.UpsertPullRequestHead).
--   checked_at   when it was read, which is what the five minutes are measured from.
--
-- A new table rather than a branch column on default_branch_commits, whose primary key is
-- (repo, sha). The release before this one writes `insert ... on conflict (repo, sha) do
-- update` and reads `where repo = $1 and sha = $2`, and a Dispatch release rolls, so both
-- builds run against one schema (cmd/dispatch/README.md, "Database migrations"). Widening that
-- key would leave the old build's `on conflict` targeting no unique constraint, so every write
-- of it would fail, and its single-row read could answer a non-default branch's yes as the
-- default branch's. Nothing in this release reads or writes default_branch_commits, which is
-- left exactly as it is; its drop is a contracting migration of a later release
-- (dispatch://INFRA-1700).
--
-- This table starts empty, with no backfill: the rows default_branch_commits holds carry no
-- branch name, and the default branch they were asked about may since have been renamed, so no
-- backfill could be faithful. Every answer is earned again with one GitHub read.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0, and rolling back past it is an image rollback alone.
create table branch_commits (
    repo text not null,
    branch text not null,
    sha text not null,
    on_branch boolean not null,
    checked_at timestamptz not null default now(),
    primary key (repo, branch, sha)
);
