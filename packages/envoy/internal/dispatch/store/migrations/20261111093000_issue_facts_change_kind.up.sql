-- 20261111093000_issue_facts_change_kind.up.sql
--
-- The delivery change log admits a sixth kind, issue_facts (dispatch://INFRA-2915). A write of the
-- stored per-issue Legion facts logs a change under it, so every view built on those rows wakes
-- the way a delivery row's write wakes the Delivery and Pipeline pages (0100). The trigger that
-- writes those rows is the migration after this one, so each of the two locks one table alone.
--
-- delivery_changes.kind carries its check inline, with no name of its own, so the check is dropped
-- by the name Postgres generated for it, delivery_changes_kind_check, and added again under that
-- name.
--
-- The new list admits every kind the old one did, so no stored row can fail it. It is added
-- `not valid` all the same, as 20261102233625 adds its widened check: a validated add reads every
-- row of the table while it holds the table's ACCESS EXCLUSIVE lock, where `not valid` changes the
-- catalogue alone and still refuses every row written or updated after it. Nothing validates it
-- later. There is nothing to validate, and the census answers 0 for the same reason.
--
-- This migration locks delivery_changes alone, for milliseconds (0073's reason).
alter table delivery_changes drop constraint delivery_changes_kind_check;
alter table delivery_changes add constraint delivery_changes_kind_check check (
  kind in ('pull_request', 'run', 'run_job', 'session_group', 'settings', 'issue_facts')
) not valid;
