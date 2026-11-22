-- 20261122110014_spec_outcome_records.up.sql — the lists of outcomes of a pull request's head.
--
-- R27 gives a change three lists of the outcomes it makes different for a caller: the author's,
-- the reviewer's own, written before the reviewer reads the author's, and the reviewer's final
-- list (D6 of the work-units plan). The ending of a change request that proposes nothing is judged
-- against the final list at the head that would land (D10). This migration creates
-- spec_outcome_records, one row per list a writer records on an issue
-- (POST /api/v1/issues/{key}/outcomes, api/change_request.go), holding id, issue_key,
-- pull_request, head, list, outcomes, actor and recorded_at.
--
-- - id is the record's own number, which the spec.outcomes_recorded event names; it rises with
--   each record, so the newest record of a set is the one of the highest id.
-- - issue_key is a foreign key to issues, on delete cascade.
-- - pull_request is the pull request the list is of, `owner/name#number` as the route stores a
--   reference it read (refs.ParseExternalRef), and head the 40 lowercase hex characters of the
--   commit; the checks hold the rules the route refuses first, so direct SQL writes no other.
-- - list is author, reviewer or final.
-- - outcomes is the list as a JSON array of {text, statement?, restored?}, in the order written;
--   an empty array is a list recorded empty.
-- - actor is the writer's actor, as handoffs.author is; the route compares its kind and id to
--   refuse an author's reviewer or final list and a reviewer's author list.
-- - recorded_at is when the row was written.
--
-- Rows are never updated: a list recorded again is a new row, and the newest final row at a head
-- is that head's final list. The index answers the route's reads, which name an issue, a list and
-- a head, or an issue and a pull request.
--
-- A new table that no earlier release names: nothing existing is read or rewritten, and the
-- release before this one never names it, so it rolls (cmd/dispatch/README.md, "Database
-- migrations") and its census answers 0. Its foreign key holds issues in SHARE ROW EXCLUSIVE only
-- while the table is created. Its identifier is the UTC moment it was written, as `migrationid`
-- requires of every migration past the numbered series (cmd/dispatch/README.md, "Database
-- migrations").
create table spec_outcome_records (
  id bigint generated always as identity primary key,
  issue_key text not null references issues(key) on delete cascade,
  pull_request text not null check (pull_request ~ '^[^/[:space:]]+/[^/#[:space:]]+#[1-9][0-9]*$'),
  head text not null check (head ~ '^[0-9a-f]{40}$'),
  list text not null check (list in ('author', 'reviewer', 'final')),
  outcomes jsonb not null check (jsonb_typeof(outcomes) = 'array'),
  actor jsonb not null,
  recorded_at timestamptz not null default now()
);

create index spec_outcome_records_issue_list_head on spec_outcome_records (issue_key, list, head, id);
