-- 20261117100000_issue_facts.up.sql
--
-- The per-issue Legion facts as stored rows, one row per project and issue
-- (dispatch://INFRA-2915, dispatch://INFRA-2916): what the Legion board, the issue table and the
-- Legion evals' day rows read, so a read of them reads rows and computes nothing. The Legion
-- facts consumer (legionfacts.Run) recomputes an issue and upserts its row when that issue's
-- inputs move. An open issue's facts grow with the clock while no input moves, so a row holds its
-- facts closed at the newest input they read and how fast each grows after it (as_of and growth,
-- below), and a read gives each fact at its own clock (legionfacts.Row.At): no timer rewrites a
-- row.
--   project_key     the project the issue belongs to, and issue_key the issue itself. The two
--                   are the primary key, which is also the read of one project's rows.
--   opened_at       when the issue was created, and closed_at when it closed, or null while it
--                   is open. Both come from the issue's own row in the issues table.
--   hours_by_phase  the issue's work hours by board column, as legion/metrics.ProjectFactsClosed
--                   shares them out, each hour counted once. A column with no hours is absent,
--                   so an issue whose work took no measured time holds an empty object and
--                   still has a row: a row means Legion worked the issue.
--   work_hours      the work time those hours come out of, which is metric 1's own number
--                   (legion/metrics.IssueFacts.WorkHours).
--   review_rounds   how many times the issue entered reviewing, the same count metric 3 reports.
--   root_key        the root of the issue's tree (legion.TreeRoot, as the facts computation
--                   answers it). An issue that is its own tree's root holds its own key.
--   tree_depth      how many child_of links lie between the issue and that root, zero for the
--                   root itself, walked over the population's parent links.
--   slot_hours      how long the issue has held an admission slot of its own, measured from the
--                   history of its `slot` observations (over legion_observations).
--   waiting_reason  why the issue waits — ready, slot, blocked or person — or null where it
--                   waits on nothing, by the card rule the board draws.
--   work_first_at   the first and last moment of the issue's measured work, which the board
--   work_last_at    serves. Both are null until the issue has measured work.
--   updated_at      when the consumer last wrote the row, which is how fresh the served facts
--                   are.
-- The columns after updated_at are the per-issue values the six Legion evals fold (R17): each is
-- the part the metrics' window aggregation takes from one issue, computed once by
-- legion/metrics.ProjectFactsClosed and stored, so an eval's day rows fold stored values and
-- recompute no rule.
--   parked_hours     the hours the work time took out of the issue's life for backlog or
--   queued_hours     icebox, and for standing admitted with no slot after its first worker
--                    event: the rest of metric 1's row beside work_hours.
--   started_at       the issue's first worker event, where its work time starts, null for an
--                    issue with none; merged_at its pull request's merge, null where Dispatch
--                    holds none.
--   bars             the issue's part of each of the seven bars of metric 2, keyed by role
--                    (architect, planner, implementer, tester, reviewer, merger, no_role), each
--                    {hours, holds_hours, segments} with the five segments waiting_on_person,
--                    in_a_turn, ci_wait, waiting_on_children and idle (metrics.BarPart). Every
--                    bar is present, so {} is a row no pass of the consumer has written.
--   idle_by_role     for each of the six roles, the hours of the issue's worked spans in which it
--                    held its own slot, its observed phase was in that role's bar and the role's
--                    newest claim state was not `working` (D3), every role present.
--   slot_wait_hours  the hours of the issue's observed life in which its phase was `admitted`
--                    and it held no slot, parked hours left out (D3).
--   sent_back        whether it moved back to implementing from testing, reviewing, accepting
--                    or the merger's phases. With the two below, the rest of metric 3's row
--                    beside review_rounds.
--   implementing_after_first_review, testing_after_first_review
--                    the implementing and the testing hours of its bar spans after its first
--                    review.
--   children         how many issues of the project have it as their tree root.
--   repo             the repository the issue resolves to by the server's one rule (its own,
--                    the nearest one up its parent chain, else its project's default mapping,
--                    refs.IssueRepoTerms), and null where none resolves, as of the pass that
--                    wrote the row. No read shows it or cuts by it: the day rows and the eval
--                    items route resolve the issue when they read it. A pass's write compares it
--                    with the repository the pass resolved, so a move of the issue's repository
--                    is a change of its row, which makes the day-row refresher rebuild a closed
--                    issue's day under the repository it moved to.
-- The last three let a read give each fact at its own clock, and name the build that wrote the
-- row (R21, R27):
--   as_of          the moment the row's values are closed at: the newest input the issue's facts
--                  read (legion/metrics.ProjectFactsClosed). Every part of the issue still
--                  running then - its current phase, its current segment, its current wait -
--                  began at or before it, and no clock moves the values, so a recomputation whose
--                  inputs did not move writes the same row and logs no change. Null on a row
--                  whose writer gave none, which a reader closes at the row's updated_at.
--   growth         how fast each fact grows after as_of, in hours an hour, by the fact's name
--                  (metrics.Growth: `work_hours`, `phase_hours.implementing`,
--                  `bars.implementer.segments.idle`, `idle_by_role.reviewer`, and `work_last_at`
--                  at 1 for a bar still open, whose end runs with the clock). A read adds each
--                  rate times the hours from as_of to its own clock (legionfacts.Row.At), which
--                  is the one computation a read makes. {} for a closed issue, whose facts stand
--                  still, but for a slot it never gave up.
--   written_build  the build that wrote the row (legionfacts.Deps.Build), '' on a row no pass of
--                  the consumer wrote. The build a project's record of its whole read names
--                  (issue_facts_computed.computed_build, 20261117100003) computes again every row
--                  of that project another build wrote, which is how a roll ends with no timer:
--                  the older build's last write logs a change, whose drain wakes the newer
--                  process, whose pass computes that row again (legionfacts.otherBuildsRows).
--
-- Four indexes serve the reads that are not of one project's whole primary-key range:
--   issue_facts_closed, (project_key, closed_at desc), is the read of the issues a project closed
--     in a window, from which the Legion evals' day rows are built (`... from issue_facts where
--     project_key = $1 and closed_at >= $2 and closed_at < $3`, legionfacts.ReadClosedIn): one
--     project, then its closes newest first, so one index range answers a window. The measurement
--     of the root design read 111 rows of a 28-day window through it in 1.6 to 2.6 ms over 28,000
--     rows (dispatch://INFRA-979/artifact/review-storage-measured-md, section 4). It is also the
--     board's read of a project's open rows, its range where closed_at is null.
--   issue_facts_root, (project_key, root_key), is the board's read of the rows of the trees it
--     shows (R22, legionfacts.Read): the board serves the facts of every open issue, through
--     issue_facts_closed, and of the issues it shows, which are the issue of each card, read by
--     key through the primary key, and every issue of a tree a card stands in, read by that tree's
--     root through this index. Without it the read of a tree would be the project's whole range of
--     the primary key with root_key filtered after it, which grows with every issue the project's
--     Legion work ever covered.
--   issue_facts_written_build, (project_key, written_build), is the read of the rows another build
--     wrote (legionfacts.otherBuildsRows), taken as two ranges of each project: the rows whose
--     writer sorts before the running build and those that sort after it. A settled deployment
--     holds neither, so the read is two probes a project.
--   issue_facts_merged, (merged_at) over the rows with a merge, is the consumer's read of the
--     stored rows a pull request's change left with a merge no pull request backs
--     (legionfacts.lostMerges). A stored row's merged_at is the earliest merge of its issue's pull
--     requests, and the attribution moves a pull request to another issue, or to none, by
--     rewriting its issue_key (delivery.AttributePullRequests). The change row that write logs
--     names the repository, the number and the merge times alone (0100), so the issue the pull
--     request left is found by the merge it held before the change: `... from issue_facts where
--     merged_at = any($1) and merged_at is not null`, each merge a probe of this index answering
--     the rows stored with that merge. Without it the read is a scan of the whole table, every
--     project's every row, on each page of changes that holds a pull request's. The rows with no
--     merge (every open issue's, and every issue's no pull request names) are left out, as
--     delivery_pull_requests_merged_at (0080) leaves them out of the pull requests.
-- Each index is a cost on every row the consumer upserts, and each serves a statement of this
-- release.
--
-- issue_facts_cursor holds one row per named cursor: where the consumer of several feeds stopped
-- in each, so a restart takes up where the last pass left off rather than reading a feed again
-- from its start. updated_at is, on the `events` and `legion_observations` rows, the moment the
-- pass that last wrote the row ran at, which every pass stamps whether or not its feed held
-- anything new, and on the `delivery_changes` row when a page last read the change log to its end
-- (delivery.ChangeCursor's ReadAt), which a full page leaves where it stood. at is, on the
-- `delivery_changes` row, when the change at that position was recorded (its
-- delivery_changes.at), and null on the other rows and for a position no change holds: a pass that
-- finds the change at its cursor gone from the log, or another change at its seq, reads the two to
-- tell a change pruned after its 24 hours (delivery.PruneChanges), which loses nothing, from a log
-- that lost changes the cursor never read (delivery.CursorLost).
--
-- Two new tables and their indexes, all built over no row: nothing existing is read, locked or
-- rewritten, and the release before this one neither reads nor writes either table, so the two
-- builds roll together (cmd/dispatch/README.md, "Database migrations"). The census answers 0, and
-- rolling back past it is an image rollback alone.
create table issue_facts (
    project_key text not null,
    issue_key text not null,
    opened_at timestamptz,
    closed_at timestamptz,
    hours_by_phase jsonb not null default '{}'::jsonb,
    work_hours double precision not null default 0,
    review_rounds integer not null default 0,
    root_key text not null default '',
    tree_depth integer not null default 0,
    slot_hours double precision not null default 0,
    waiting_reason text,
    work_first_at timestamptz,
    work_last_at timestamptz,
    updated_at timestamptz not null default now(),
    parked_hours double precision not null default 0,
    queued_hours double precision not null default 0,
    started_at timestamptz,
    merged_at timestamptz,
    bars jsonb not null default '{}'::jsonb,
    idle_by_role jsonb not null default '{}'::jsonb,
    slot_wait_hours double precision not null default 0,
    sent_back boolean not null default false,
    implementing_after_first_review double precision not null default 0,
    testing_after_first_review double precision not null default 0,
    children integer not null default 0,
    repo text,
    as_of timestamptz,
    growth jsonb not null default '{}'::jsonb,
    written_build text not null default '',
    primary key (project_key, issue_key)
);
create index issue_facts_closed on issue_facts (project_key, closed_at desc);
create index issue_facts_root on issue_facts (project_key, root_key);
create index issue_facts_written_build on issue_facts (project_key, written_build);
create index issue_facts_merged on issue_facts (merged_at) where merged_at is not null;

create table issue_facts_cursor (
    name text primary key,
    position bigint not null default 0,
    at timestamptz,
    updated_at timestamptz not null default now()
);
