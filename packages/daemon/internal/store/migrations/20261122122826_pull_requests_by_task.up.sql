-- 20261122122826_pull_requests_by_task.up.sql — the pull request of each task of an issue:
-- pull_requests keyed by the issue and the branch, and the outbox kind that closes the other pull
-- requests of an issue (task_pull_requests_close).
--
-- The table held one row for each issue, keyed by issue (0004); each task of an issue now keeps
-- its own pull request on its own branch, so the key is (issue, branch), and 0004's unique
-- pull_requests_repo_branch stays. The table stores no task: a pull request's task is the task of
-- its issue's plan at the issue's generation whose branch the pull request is on, which the record
-- reads from plan_tasks at each read (record.PullRequest.Task), so no row can hold a task that a
-- branch given to a task after the row was written contradicts. Each row the upgrade finds keeps
-- its issue and its branch, which is legion/<KEY>, so it is the pull request of the task that holds
-- that branch: the one task of a plan of one, which the release before ran (plan.Supported refused
-- more).
--
-- The outbox check is one list, so this migration names every kind, as 20261121165047 did: its
-- thirty-one kinds and task_pull_requests_close. Its version stands above 20261121165047 on purpose:
-- Migrate applies every unrecorded migration in version order, so the list with the higher version
-- is the last word on any database.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack), run with the daemon stopped, before the
-- earlier image is pinned, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1; a rollback past
-- 20261121165047 too runs this block before that migration's own. The release before writes each
-- row with `on conflict (issue)`, which needs the key of one row for each issue, and decodes no
-- task_pull_requests_close row. So the block deletes each such row first, since the check written
-- again without the kind refuses a row that holds it; then keeps, of each issue, the row on the
-- branch of the task that merges (the one task of its plan at its generation that no task needs,
-- on the branch its row holds or on legion/<KEY> where it holds none) or on legion/<KEY> where the
-- issue has no plan rows there, and deletes the others; keys the table by the issue again; and
-- deletes its version row, which is owed to the roll-forward, so that it runs this file again. What
-- the way back costs is the row of each task's pull request but the one that merges: the record
-- holds it no longer, so a later event of that pull request names no row.
--
-- begin;
-- delete from outbox where kind = 'task_pull_requests_close';
-- alter table outbox drop constraint if exists outbox_kind_check;
-- alter table outbox add constraint outbox_kind_check check (kind in (
--   'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
--   'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
--   'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
--   'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
--   'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
--   'conflict_confirm', 'merge_forward', 'design_changes', 'task_branch'));
-- delete from pull_requests pr where pr.branch <> coalesce((select coalesce(nullif(t.branch, ''), 'legion/' || t.issue) from plan_tasks t join issues i on i.key = t.issue and i.generation = t.generation where t.issue = pr.issue and not exists (select 1 from plan_tasks u where u.issue = t.issue and u.generation = t.generation and t.task = any(u.needs)) order by t.task limit 1), 'legion/' || pr.issue);
-- alter table pull_requests drop constraint pull_requests_pkey, add constraint pull_requests_pkey primary key (issue);
-- delete from schema_version where version = 20261122122826;
-- commit;
alter table pull_requests drop constraint pull_requests_pkey,
  add constraint pull_requests_pkey primary key (issue, branch);

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'task_branch', 'task_pull_requests_close'));
