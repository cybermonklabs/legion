-- 20261119003157_conflict_reads_and_merge_forward.up.sql — a head that conflicts with its base is
-- read twice before the daemon acts on it, and the act asks the implementer to merge the base
-- forward while the issue keeps its phase, rather than sending the issue back to implementing
-- (dispatch://INFRA-4374 R1, R2).
--
-- The outbox kind check takes two new kinds. conflict_confirm is the second read of a pull request
-- whose first read found its head CONFLICTING, due a minute after that first read
-- (record.PullRequest.ConflictRead). The fact that made the first read is acknowledged once its
-- transaction commits, so the second read is a durable row rather than a read held in memory, as
-- merge_queue_read is (20261114031356). merge_forward is one message to one role about a confirmed
-- conflict the daemon acted on (record.PullRequest.MergeForward): the ask to the implementer, the
-- hold to the worker of the phase the issue stays in, and, once a new head lands, the word that it
-- landed and the reviewer's ask to read the merge.
--
-- The check is one list, so the migration that sets it names every kind, as 20261117023932 did:
-- that migration's twenty-seven kinds, plan_record among them, and these two. Its version stands
-- above 20261117023932 on purpose: Migrate applies every unrecorded migration in version order,
-- so the list with the higher version is the last word on any database, and a list that left out
-- plan_record would refuse every plan-record row the daemon queues from then on.
--
-- phase_ended stays in the list, though this release queues no row of it. The release before
-- queued one for the role whose phase a conflict ended, when that conflict sent the issue back to
-- implementing. After a rollback that release runs on the check this file writes, since it has no
-- migration of its own left to apply, and a list without phase_ended would refuse the first such
-- row it queues (SQLSTATE 23514), and the transaction of the move that queued it with it.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward'));

-- Every phase_ended row still queued goes. Each is a message the release before queued about a
-- move it made, a conflict that sent the issue back to implementing, to the role whose phase that
-- move ended. This release neither makes that move nor delivers that message: a conflict keeps the
-- issue in its phase, so the role the row would tell still holds its phase, and the row would tell
-- it the opposite.
delete from outbox where kind = 'phase_ended';

-- pull_requests.conflict_read is record.PullRequest.ConflictRead: the first CONFLICTING read of
-- the record's head that no second read has confirmed or released yet, with the head, the base
-- branch and the time of that read. pull_requests.merge_forward is record.PullRequest.MergeForward:
-- the daemon's ask that the implementer merge the base forward onto a head whose conflict was
-- confirmed, with that head and base, the phase the issue stays in, why the daemon acted, when it
-- asked, the first head that landed after the ask, whether the reviewer was asked to read the
-- merge, and each merge forward that landed before it while the issue stayed out of implementing.
-- Both are nullable jsonb, null where the record holds none, which is every row the table
-- holds now: no release before this one read a conflict twice or asked for a merge forward, so
-- nothing fills them. A new generation clears merge_forward, an ask of the run before, and keeps
-- conflict_read, GitHub's answer about the head and the base (record's clearGeneration).
alter table pull_requests add column conflict_read jsonb, add column merge_forward jsonb;

-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists. The release before names each column of pull_requests it reads and
-- writes (record's pullRequestColumns and PutPullRequest), so it neither reads nor writes these
-- two, and the check above takes every kind it queues. What it cannot do is run a row of the two
-- new kinds, which no lane of it takes, or clear the two columns, which it never names. So the way
-- back is the one transaction below, run with this release's daemon stopped and before the release
-- before starts:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   delete from outbox where kind in ('conflict_confirm', 'merge_forward');
--   update pull_requests set conflict_read = null, merge_forward = null
--     where conflict_read is not null or merge_forward is not null;
--   commit;
--   SQL
--
-- The delete costs each first read its second read and each role one message about one merge
-- forward; the release before reads mergeability its own way and acts on a conflict at its first
-- read. The update is owed to the roll-forward: the release before moves heads and issues without
-- touching either column, so by the roll-forward a first read or an ask it left standing would
-- name a head and a phase that its own moves have overtaken.
--
-- The roll-forward after such a rollback takes one statement of its own, run with the release
-- before stopped and before this release starts. That release queues phase_ended rows again, and
-- this file, whose version the database still records, does not run again to delete them:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 -c "delete from outbox where kind = 'phase_ended'"
