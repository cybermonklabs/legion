-- 20261124130001_pull_request_reviews_requested.up.sql — what the read that recorded a pull
-- request's merge state found of the two holds of a `blocked` head the daemon can read without a
-- ruleset: how many review requests GitHub holds open on the pull request
-- (record.PullRequest.ReviewsRequested), and whether a check or a commit status of the head was
-- still running (record.PullRequest.HeadChecks). A REST read of one pull request counts its
-- `requested_reviewers` and `requested_teams`, and reads the head commit's `statusCheckRollup`
-- from GitHub's GraphQL API when it answers `blocked`; a base's GraphQL batch reads
-- `reviewRequests { totalCount }` and the same rollup beside the merge state. Both are cleared and
-- carried with merge_state.
--
-- Why: GitHub's merge state is its answer to the reader, not its verdict on the merge, so the
-- review round ends on `blocked` once the head is MERGEABLE, no check of it is running, its own
-- checks have concluded and no review is asked for, and the merger's enqueue is what GitHub
-- answers about the merge (classify.MergeOutcomeOf,
-- dispatch://INFRA-4192/comment/1c89885e-5e89-42f7-8122-9cdc697d0ccd).
--
-- Every existing row takes the value that says no read wrote it: reviews_requested null and
-- head_checks ''. The round ends on `blocked` only on values a read wrote, so a round an earlier
-- release recorded `blocked` waits for the daemon's next read of the pull request, which writes
-- both.
--
-- There is no down file: migrations here are forward-only. An earlier release neither reads nor
-- writes either column, and its insert names the columns it writes, so its rows take null and '';
-- a rollback needs nothing put back.
alter table pull_requests
  add column reviews_requested integer check (reviews_requested >= 0),
  add column head_checks text not null default '' check (head_checks in ('', 'running', 'concluded'));
