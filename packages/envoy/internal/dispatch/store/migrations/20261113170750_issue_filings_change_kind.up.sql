-- 20261113170750_issue_filings_change_kind.up.sql
--
-- The delivery change log admits one more kind, issue_filings (dispatch://INFRA-2916, R12). A
-- write of a stored filing (issue_filings, 20261113082155) logs a change under it, so the day-row
-- refresher (evals.Store.RunRefresher) rewrites the splitting rows of the filing's day. The Legion facts
-- consumer writes a filing when its `issue.created` event reaches it, and rewrites it when the
-- filing's class, its filer's phase or the filed issue's tree root moves. None of those writes
-- moves an issue_facts row, so without a kind of their own no change would say that a filing day's
-- input moved. The trigger is the next migration, so each of the two locks one table alone.
--
-- The list is every kind 20261113082159 admitted, its nine, and the new one. A migration that adds
-- a kind after this one lists all ten and its own, since the check is replaced whole.
--
-- The new list admits every kind the old one did, so no stored row can fail it. It is added
-- `not valid`, as 20261113082159 adds the list before it: a validated add reads every row of the
-- table while it holds the table's ACCESS EXCLUSIVE lock, where `not valid` changes the catalogue
-- alone and still refuses every row written or updated after it. Nothing validates it later, and
-- the census answers 0 for the same reason. The release before this one writes none of the new
-- kind, so the two roll together.
--
-- This migration locks delivery_changes alone, for milliseconds (0073's reason).
alter table delivery_changes drop constraint delivery_changes_kind_check;
alter table delivery_changes add constraint delivery_changes_kind_check check (
  kind in ('pull_request', 'run', 'run_job', 'session_group', 'settings', 'issue_facts',
           'eval_readings', 'eval_versions', 'eval_days', 'issue_filings')
) not valid;
