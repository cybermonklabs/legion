-- 20261119173616_commit_pull_reads.up.sql
--
-- The moment before which no process reads GitHub again for the pull requests of one commit of
-- one repository: the read-through of a result's commit that the results read makes
-- (one POST /graphql for up to 20 commits of a repository, and GET
-- /repos/{owner}/{repo}/commits/{sha}/pulls for a commit GitHub names more pull requests for than
-- that query reads; delivery.CommitPullReads.ClaimAcrossProcesses, api/results_read_through.go
-- readThroughHeads). Each process kept that moment in its own memory, so N processes behind one
-- load balancer read one commit through up to N times a span, and this release reads every
-- result's commit through, also one that a stored pull request heads. A row here is the one claim
-- every process shares.
--
--   repo        the repository, under the canonical name the results read resolved
--               (refs.CanonicalRepo), as the process's memory keys it.
--   sha         the commit.
--   not_before  the moment before which no process may read the pair through again: five
--               minutes past the read a claim granted (delivery.CommitPullReadStaleAfter), or an
--               hour past GitHub's answer that the pull request's head is another commit
--               (delivery.CommitPullSettledFor). A row whose moment has passed refuses nothing.
--   answered    whether an answer of GitHub's stands behind the claim: the read it was granted
--               for stored GitHub's answer about the commit (delivery.CommitPullReads
--               .AnswerAcrossProcesses, and a settle). A read the claim refuses reads the commit
--               from the stored rows only where it is true; where it is false the read behind the
--               claim is under way, failed at GitHub or stopped with its process, and a result at
--               the commit is not known (read_failed). A granted claim sets it false.
--
-- One statement grants a claim: an insert of the pair that, on conflict, moves the moment only
-- where the moment held has passed, so two processes that ask at once are granted once. A granted
-- claim then deletes the rows whose moment has passed, in a statement of its own, so the table
-- holds about one row for each commit asked about in the last hour. Each statement locks the rows
-- it writes, for that statement alone; nothing else writes the table, and the results read alone
-- reads it (RefusesAcrossProcesses, HeldAcrossProcesses).
--
--   commit_pull_reads_not_before  the rows whose moment has passed, which each claim deletes.
--
-- A new table nothing before this release names: the release before this one neither reads nor
-- writes it, so the two roll side by side (cmd/dispatch/README.md, "Database migrations"), and
-- the census answers 0 because the table holds no row at this schema. During the roll the release
-- before keeps its own memory alone, so it can read a commit through once more a span. Rolling
-- back past this migration is an image rollback alone.
create table commit_pull_reads (
    repo text not null,
    sha text not null,
    not_before timestamptz not null,
    answered boolean not null default false,
    primary key (repo, sha)
);

create index commit_pull_reads_not_before on commit_pull_reads (not_before);
