-- 20261113111231_issue_facts_parts.up.sql
--
-- Widens the stored per-issue Legion facts (20261111090000, dispatch://INFRA-2915) with the
-- per-issue values the six Legion evals fold (dispatch://INFRA-2916, R17): each is the part the
-- metrics' window aggregation takes from one issue, computed once by legion/metrics.ProjectFacts
-- and stored, so an eval's day rows fold stored values and recompute no rule. hours_by_phase
-- stays as it is, because the Legion board reads it.
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
--                    bar is present, so {} is a row no build of this release has written.
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
--                    wrote the row: an eval item shows it, while the day rows and the items
--                    route's repo filter resolve the issue again when they read it.
--
-- Twelve columns in one statement, so issue_facts is locked once. Each is nullable or carries a
-- constant default, which Postgres records in the catalog and writes into no stored row; the
-- statement reads no row and refuses none, and its census answers 0. issue_facts holds one row
-- per issue Legion worked, far below the census's 1 GiB bound (pgmigrate.CensusTableLimit), and
-- the ACCESS EXCLUSIVE the statement takes lasts as long as the catalog change, under the
-- runner's five-second bound on the wait.
--
-- The release before this one reads and writes issue_facts by naming its columns, so it goes on
-- working beside this one (cmd/dispatch/README.md, "Database migrations"): a row it inserts takes
-- the defaults, and a row it updates keeps the values this release wrote. Every row is written
-- whole by this release's first pass: the Legion facts consumer reads a project whole on the
-- first pass of a build other than the one its record names (20261112193821,
-- legionfacts.legionProjects), and its write compares every column, these among them, so each
-- row whose new values differ from the defaults is rewritten then. While the two releases roll,
-- each pass of either build rewrites the rows under its own rule, as that record already says.
-- Rolling back past this migration is an image rollback alone.
alter table issue_facts
    add column parked_hours double precision not null default 0,
    add column queued_hours double precision not null default 0,
    add column started_at timestamptz,
    add column merged_at timestamptz,
    add column bars jsonb not null default '{}'::jsonb,
    add column idle_by_role jsonb not null default '{}'::jsonb,
    add column slot_wait_hours double precision not null default 0,
    add column sent_back boolean not null default false,
    add column implementing_after_first_review double precision not null default 0,
    add column testing_after_first_review double precision not null default 0,
    add column children integer not null default 0,
    add column repo text;
