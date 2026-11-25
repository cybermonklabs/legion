-- 20261120144431_pull_request_heads_attribution_inputs.up.sql
--
-- What a pull request's issue was judged from, stored on its pull_request_heads row, so that a
-- change of an issue's links judges the row again from the database alone, with no read of GitHub
-- (delivery.LinksChanged, called by PATCH /api/v1/issues/{key}). A link a person makes by hand is
-- answered from what Dispatch holds.
--
--   attribution_title_keys     the issue keys the title and the body name, in first-seen order
--   attribution_cited_issues   the GitHub issue URLs the body cites
--                              (https://github.com/<owner>/<repo>/issues/<n>, a bare #N in the pull
--                              request's own repository), in body order
--   attribution_branch_keys    the issue keys the head branch's name names
--   attribution_commit_keys    the issue keys the commit messages name, null where no read of them
--                              was made: they are read only when no earlier source names an issue
--                              the pull request may keep, so null is "not known" and not "none"
--   head_in_repo               whether the head branch is in the pull request's own repository; a
--                              head in a fork has no issue, linked or not
--
-- They are the inputs delivery_pull_requests already stores for a merged pull request
-- (delivery.AttributionInputs), and each writer of a row that read GitHub writes them with the
-- row's issue, from the read that judged it (delivery.UpsertPullRequestHead). Each list is never
-- null where a writer read it: an empty list is a source that names nothing.
--
-- Every row stored before this migration reads null in all five, and no backfill writes them here:
-- the body and the commit messages are on no stored row, and a census that rewrites a row refuses
-- the deploy (cmd/dispatch/README.md, "Pre-deploy census"). The release that carries this migration
-- raises delivery.AttributionRuleVersion to 4, so the reconcile's merged arm reads each merged row
-- once more and stores its inputs, at most delivery.RejudgedHeadsPerPass (50) rows a pass, about 600
-- an hour. An open row is read again by the first reconcile pass that finds it unread for longer
-- than delivery.PullRequestHeadStaleAfter (four and a half minutes), as far as the pass's batch of
-- delivery.PullRequestHeadBatch (200) rows reaches, so two reads of it are at most about nine and a
-- half minutes apart. Until then a change of the links judges such a row as the release before did:
-- it takes the pull request from the issue where the link that gave it is gone, and leaves it for
-- the reconcile.
--
-- Adding a nullable column with no default is a catalog change that rewrites no row. The ALTER
-- takes ACCESS EXCLUSIVE on pull_request_heads, which every pull request event and every results
-- read touch, and holds it only for the catalog write. It must first wait for every transaction
-- that holds the table: the runner bounds that wait at pgmigrate.LockTimeout (five seconds) and
-- tries again up to pgmigrate.LockTimeoutAttempts times, after which the migration fails the boot.
-- No index is added: the one reader by these columns is a person's change of an issue's links,
-- which scans the table's few thousand rows once.
--
-- The release before this one neither reads nor writes the columns, and its insert names its
-- columns, so the two roll side by side (cmd/dispatch/README.md, "Database migrations"): a row
-- that release writes during the roll reads null here and is judged as a row stored before this
-- migration is. Rolling back past this migration is an image rollback alone; the columns left
-- behind are read by nothing. The rollback script beside this file drops them, for an operator
-- who wants them gone once no release that writes them serves.
alter table pull_request_heads
    add column attribution_title_keys text[],
    add column attribution_cited_issues text[],
    add column attribution_branch_keys text[],
    add column attribution_commit_keys text[],
    add column head_in_repo boolean;
