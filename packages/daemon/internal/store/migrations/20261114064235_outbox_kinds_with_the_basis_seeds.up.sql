-- 20261114064235_outbox_kinds_with_the_basis_seeds.up.sql — the two outbox kinds that read
-- Dispatch for a root's design-gate basis and for a re-parented root (dispatch://INFRA-3320).
--
-- pending_basis_seed reads the ancestor's open approval request a root's pending step waits on
-- (record.PendingBasisSeed) and applies what it finds: the person's approval opens the step, a
-- changes request, a newer version or the request's close with no approval ends the wait, and a
-- request still waiting changes nothing. reparent_seed reads the new Dispatch parent of a root that
-- ran as a tree of its own (record.ReparentSeed): the engine does no I/O, so the message to that
-- root and the notice to its parent's tree's architect wait on this read of whether the parent
-- marks requirement blocks and holds a person's approval of its latest version.
--
-- The check is one list, so the migration that sets it names every kind, as 20261106041346,
-- 20261108000545, 20261110235320, 20261113082214 and 20261114031356 did: whichever of two open
-- branches merges last admits both their kinds. The list is 20261114031356's twenty-four kinds,
-- merge_queue_read and merge_queue_removal among them, with pending_basis_seed and reparent_seed
-- added. It drops the constraint if it exists before adding it, so it is safe on a database at
-- either branch. Its version stands above 20261114031356, and Migrate applies every unrecorded
-- migration in version order, so this list is the last word on a fresh database and on one main
-- migrated. No release carries this file, so no deployment's database records it. A database a
-- build of this branch migrated before the forward merge that brought 20261114031356 in does
-- record it and not 20261114031356, so its next upgrade applies that file after this one: a
-- queued pending_basis_seed or reparent_seed row then stops the upgrade (SQLSTATE 23514), and
-- with none queued the check it leaves refuses both kinds, since this file does not run again.
-- Run this file's two statements by hand on such a database after its upgrade.
--
-- A daemon of an earlier release refuses neither row at the database, since this list admits
-- them, but its strict decoder knows neither kind, so a row of either it finds pending fails its
-- decode on every attempt.
--
-- There is no down file: migrations here are forward-only.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'pending_basis_seed', 'reparent_seed'));
