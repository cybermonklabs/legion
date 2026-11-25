-- 20261124130001_pull_request_reviews_requested.up.sql — what the read that recorded a pull
-- request's merge state found of the holds of a head the daemon can read without a ruleset: how
-- many review requests GitHub holds open on the pull request (record.PullRequest.ReviewsRequested),
-- whether a check or a commit status of the head was still running
-- (record.PullRequest.HeadChecks), how many reads of that head in a row GitHub answered `mergeable`
-- null and when the first of them was made (record.PullRequest.NullMergeableReads and
-- NullMergeableSince), and the read time of the read that wrote them
-- (record.PullRequest.EvidenceReadAt). A base's GraphQL batch reads the count as
-- `reviewRequests { totalCount }` and the head commit's `statusCheckRollup`, counted by state,
-- beside the merge state; a REST read of a `blocked`, MERGEABLE head takes its whole answer from
-- the same GraphQL query; any other REST read counts its `requested_reviewers` and
-- `requested_teams`.
--
-- Why: GitHub's merge state is its answer to the reader, not its verdict on the merge, so the
-- review round ends on `blocked` once the head is MERGEABLE, no check of it is running, its own
-- checks have concluded and no review is asked for, and the merger's merge is what GitHub answers
-- about the merge (classify.MergeOutcomeOf,
-- dispatch://INFRA-4192/comment/1c89885e-5e89-42f7-8122-9cdc697d0ccd).
--
-- Every existing row takes the values that say no read wrote them: reviews_requested null,
-- head_checks '', null_mergeable_reads 0, and null_mergeable_since and evidence_read_at the zero
-- instant (as state_read_at of 20261112070503 takes it). The round ends on `blocked` only on values
-- whose evidence_read_at equals the row's state_read_at, so a round an earlier release recorded
-- `blocked` waits for the daemon's next read of the pull request, which writes them all.
--
-- There is no down file: migrations here are forward-only. An earlier release neither reads nor
-- writes these columns, and its insert names the columns it writes, so its new rows take the
-- unread values. Its update of a row this release wrote, at a new head or at the same one, writes
-- its own state_read_at and leaves these columns where they stood, so after a rollback and a
-- roll-forward they can be an older read's: evidence_read_at names the read that wrote them, and
-- classify reads them as unread while it differs from state_read_at. So a rollback needs nothing
-- put back.
alter table pull_requests
  add column reviews_requested integer check (reviews_requested >= 0),
  add column head_checks text not null default '' check (head_checks in ('', 'running', 'concluded')),
  add column null_mergeable_reads integer not null default 0 check (null_mergeable_reads >= 0),
  add column null_mergeable_since timestamptz not null default '0001-01-01 00:00:00+00',
  add column evidence_read_at timestamptz not null default '0001-01-01 00:00:00+00';
