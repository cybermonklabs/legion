-- 20261128022326_merged_new_work.up.sql — a request for new work on an issue whose work merged is a
-- reason of its own kind, the read of whether a closed issue's work merged is an outbox kind, and a
-- person's confirmation that an issue's work never merged is a column of its record.
--
-- defects takes the kind request (record's DefectKindRequest): the reason of the one next change a
-- request starts. A merged change carries it, as it carries a defect, and its role says who asked:
-- person, controller or architect. The close lists it apart from the defects.
--
-- The outbox takes merge_read (record's MergeRead): the read of whether a closed issue's work
-- merged, which a request waits on where the record shows no merge that proves it. The kind list is
-- the one of 20261128022325_done_by_checks with merge_read, written whole, and this version stands
-- above every migration main and this chain carry, so this list is the last word on any database.
--
-- issues.never_merged_confirmed (record's ConfirmNeverMerged) says that a person confirmed, by a
-- move to backlog, that the issue's work never merged. The next todo takes it, and only then does
-- the issue run again as a whole. Every row the upgrade finds holds false. It is the one durable
-- fact this release adds: no other fact of the record names who moved an issue.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack). Run it with every project's daemon on that
-- database stopped, before the two blocks of 20261128022325_done_by_checks. It first prints three
-- lists that a person keeps, as notices: each open request, each issue whose merge read was pending,
-- and each issue whose confirmation no todo took. An issue of the second list is closed and Dispatch
-- shows it todo, so the release before would run its whole tree again: set each to backlog before
-- the release before starts. Then it deletes what the release before cannot read: the merge reads,
-- the merge-unread notices and the requests. It puts back the two kind checks as 20261128022325
-- wrote them, drops the column, and deletes this migration's schema_version row, so the roll
-- forward applies this file again. The statement that names the column runs only where it still
-- exists, so the block run again prints nothing it cannot read.
--
-- begin;
-- do $$ begin
--   raise notice 'open requests: %', (select coalesce(json_agg(listed), '[]') from (select issue, id, role, text from defects where kind = 'request' and merged_at is null and withdrawn_at is null order by issue, id) listed);
--   raise notice 'pending merge reads: %', (select coalesce(json_agg(listed), '[]') from (select issue, payload->>'requester' as requester, payload->>'request' as request from outbox where kind = 'merge_read' order by issue, id) listed);
--   if exists (select 1 from information_schema.columns where table_schema = current_schema() and table_name = 'issues' and column_name = 'never_merged_confirmed') then
--     raise notice 'unconsumed never-merged confirmations: %', (select coalesce(json_agg(key order by key), '[]') from issues where never_merged_confirmed);
--   else
--     raise notice 'unconsumed never-merged confirmations: []';
--   end if;
-- end $$;
-- delete from outbox where kind = 'merge_read';
-- delete from outbox where kind in ('notice', 'controller_notice') and payload->>'kind' = 'merge-unread';
-- delete from defects where kind = 'request';
-- alter table defects drop constraint if exists defects_kind_check;
-- alter table defects add constraint defects_kind_check check (kind in ('check', 'production', 'defect'));
-- alter table outbox drop constraint if exists outbox_kind_check;
-- alter table outbox add constraint outbox_kind_check check (kind in ('dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed', 'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake', 'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask', 'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record', 'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed', 'merge_holds_changed', 'state_change', 'state_admission', 'state_boot', 'close_read', 'close_wait_publish'));
-- alter table issues drop column if exists never_merged_confirmed;
-- delete from schema_version where version = 20261128022326;
-- commit;
alter table defects drop constraint defects_kind_check;
alter table defects add constraint defects_kind_check check (kind in ('check', 'production', 'defect', 'request'));

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed',
  'merge_holds_changed', 'state_change', 'state_admission', 'state_boot', 'close_read', 'close_wait_publish',
  'merge_read'));

alter table issues add column never_merged_confirmed boolean not null default false;
