-- 20261113082159_eval_change_kinds.up.sql
--
-- The delivery change log admits three more kinds, eval_readings, eval_versions and eval_days
-- (dispatch://INFRA-2916, R10). A reading, a new version of a definition and a write of the stored
-- day rows each log a change under its kind, so a view built on the evals wakes when one lands,
-- the way a delivery row's write wakes the Delivery and Pipeline pages (0100). The day-row kind is
-- the one a computed eval needs: its value moves when the day rows are rewritten, after the
-- input's own change has already woken the views, so a view woken by the input alone would read
-- the old rows and never wake again. The triggers that write these rows are the three migrations
-- after this one, so each of the four locks one table alone.
--
-- The list is every kind this check admitted before, the six of 0100 and 20261111093000, and the
-- three new ones. A migration that adds a kind after this one lists all nine and its own, since
-- the check is replaced whole.
--
-- The new list admits every kind the old one did, so no stored row can fail it. It is added
-- `not valid`, as 20261111093000 adds the list before it: a validated add reads every row of the
-- table while it holds the table's ACCESS EXCLUSIVE lock, where `not valid` changes the catalogue
-- alone and still refuses every row written or updated after it. Nothing validates it later.
-- There is nothing to validate, and the census answers 0 for the same reason.
--
-- This migration locks delivery_changes alone, for milliseconds (0073's reason).
alter table delivery_changes drop constraint delivery_changes_kind_check;
alter table delivery_changes add constraint delivery_changes_kind_check check (
  kind in ('pull_request', 'run', 'run_job', 'session_group', 'settings', 'issue_facts',
           'eval_readings', 'eval_versions', 'eval_days')
) not valid;
