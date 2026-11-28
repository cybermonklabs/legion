-- 20261123002036_production_events_change_kind.up.sql
--
-- The delivery change log admits a change of the record of deploys and failures
-- (dispatch://INFRA-978, R13): kind production_events, which 20261123002037 logs for every write of a
-- production_events row, through the outbox (delivery_change_log_pending, 20261117100001), so no
-- writer of that table takes the change log's lock (6900): only the drain does, in a short
-- transaction of its own (dispatch://INFRA-2916, R19).
--
-- Both kind checks are inline and carry the names Postgres generated for them,
-- delivery_changes_kind_check and delivery_changes_pending_kind_check, so each is dropped by that
-- name and added again under it, the new list admitting every kind the old one did and this one.
-- Each is added not valid, as 20261117100001 adds the first: every row stored holds a kind the old
-- list admitted, which the new one admits too, so no row is read to prove it.
--
-- The release before this one writes no production_events row and never reads the kind. This
-- migration locks delivery_changes and delivery_changes_pending alone, for milliseconds. It reads
-- no row, and the census answers 0.
alter table delivery_changes drop constraint delivery_changes_kind_check;
alter table delivery_changes add constraint delivery_changes_kind_check check (
  kind in ('pull_request', 'run', 'run_job', 'session_group', 'settings', 'issue_facts',
           'eval_readings', 'eval_versions', 'eval_days', 'issue_filings', 'production_events')
) not valid;

alter table delivery_changes_pending drop constraint delivery_changes_pending_kind_check;
alter table delivery_changes_pending add constraint delivery_changes_pending_kind_check check (
  kind in ('issue_facts', 'eval_readings', 'eval_versions', 'eval_days', 'issue_filings', 'production_events')
) not valid;
