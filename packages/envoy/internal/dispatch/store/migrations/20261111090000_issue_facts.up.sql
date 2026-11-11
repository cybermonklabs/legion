-- 20261111090000_issue_facts.up.sql
--
-- The per-issue Legion facts as stored rows, one row per project and issue
-- (dispatch://INFRA-2915): what the board, the issue table and every eval read, so a read of
-- them reads rows and computes nothing. A consumer recomputes an issue and upserts its row when
-- that issue's inputs move, and re-runs every open issue on a sweep, because an open issue's
-- hours grow with the clock while no input moves.
--   project_key     the project the issue belongs to, and issue_key the issue itself. The two
--                   are the primary key, which is also the board's read of one project.
--   opened_at       when the issue was created, and closed_at when it closed, or null while it
--                   is open. Both come from the issue's own row in the issues table.
--   hours_by_phase  the issue's work hours by board column, as legion/metrics.ProjectFacts
--                   shares them out, each hour counted once. A column with no hours is absent,
--                   so an issue whose work took no measured time holds an empty object and
--                   still has a row: a row means Legion worked the issue.
--   work_hours      the work time those hours come out of, which is metric 1's own number
--                   (legion/metrics.IssueFacts.WorkHours).
--   review_rounds   how many times the issue entered reviewing, the same count metric 3 reports.
--   root_key        the root of the issue's tree (legion.TreeRoot, as ProjectFacts answers it).
--                   An issue that is its own tree's root holds its own key.
--   tree_depth      how many child_of links lie between the issue and that root, zero for the
--                   root itself, walked over the population's parent links (ProjectFacts).
--   slot_hours      how long the issue has held an admission slot of its own, measured from the
--                   history of its `slot` observations (ProjectFacts, over legion_observations).
--   waiting_reason  why the issue waits — ready, slot, blocked or person — or null where it
--                   waits on nothing, by the card rule the board draws.
--   work_first_at   the first and last moment of the issue's measured work. They name the days
--   work_last_at    an issue's work falls in, which is what a reader of per-day rows rewrites
--                   when the facts move. Both are null until the issue has measured work.
--   updated_at      when the consumer last wrote the row, which is how fresh the served facts
--                   are. The row holds the facts as of that moment, never as of the read.
--
-- issue_facts_closed serves the read of the issues that closed in a window, newest first. The
-- primary key serves the read of one project's rows.
--
-- issue_facts_cursor holds one row per named cursor: where the consumer of several feeds stopped
-- in each, so a restart takes up where the last pass left off rather than reading a feed again
-- from its start.
--
-- Two new tables: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes either of them, so the two builds roll together (cmd/dispatch/README.md,
-- "Database migrations"). The census answers 0, and rolling back past it is an image rollback alone.
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
    primary key (project_key, issue_key)
);
create index issue_facts_closed on issue_facts (project_key, closed_at desc);

create table issue_facts_cursor (
    name text primary key,
    position bigint not null default 0,
    updated_at timestamptz not null default now()
);
