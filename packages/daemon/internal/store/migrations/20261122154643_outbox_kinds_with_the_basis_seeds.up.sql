-- 20261122154643_outbox_kinds_with_the_basis_seeds.up.sql — the two outbox kinds that read
-- Dispatch for a root's design-gate basis and for a re-parented root, and the kind that tells a
-- parent's stopped worker that its phase ended (dispatch://INFRA-3320).
--
-- pending_basis_seed reads the ancestor's open approval request a root's pending step waits on
-- (record.PendingBasisSeed) and applies what it finds: the person's approval opens the step, a
-- changes request, a newer version or the request's close with no approval ends the wait, and a
-- request still waiting changes nothing. reparent_seed reads the new Dispatch parent of a root that
-- ran as a tree of its own (record.ReparentSeed): the engine does no I/O, so the message to that
-- root and the notice to its parent's tree's architect wait on this read of whether the parent
-- marks requirement blocks and holds a person's approval of its latest version.
--
-- phase_ended is a kind main already lists. The release before this one queues no row of it, and
-- its migration 20261119003157 deleted every row an earlier release had queued for a conflict. This
-- release queues it again for one move alone: the admission pass's move of a parent whose worker
-- is in the middle of its phase into awaiting_children, which tells that worker its phase ended
-- (record.PhaseEnded, dispatch://INFRA-3320 R10). A row that names no dropped merge forward has
-- the shape that kind always had. A row of a move that dropped a merge forward before it landed
-- also carries `mergeForward`, its head, its base and the role it was asked of, which no earlier
-- release knows: each decodes a payload strictly, so it refuses such a row.
--
-- The check is one list, so the migration that sets it names every kind, as 20261117023932,
-- 20261119003157 and 20261119203047 did: whichever of two open branches merges last admits both
-- their kinds. The list is the design news's (20261119203047, dispatch://INFRA-175): the
-- twenty-nine kinds of 20261119003157, plan_record, conflict_confirm and merge_forward among them,
-- and design_changes, with pending_basis_seed and reparent_seed added. It drops the constraint if
-- it exists before adding it, so it is safe on a database at either branch. Its version stands
-- above 20261119203047, and Migrate applies every unrecorded migration in version order, so this
-- list is the last word on a fresh database and on one main migrated. A list that left out
-- conflict_confirm or merge_forward would refuse every second read of a conflicting head and every
-- message of a merge forward, and one that left out design_changes every reconcile of a tree's
-- news, and the upgrade of a database main migrated that holds one pending. No release carries
-- this file, so no deployment's database records it.
--
-- A database a build of this branch migrated earlier recorded this list under an older version:
-- 20261114064235 before 2026-11-17, 20261117110810 from then until 2026-11-19, and
-- 20261119185013 from then until the forward merge of 2026-11-22 brought in 20261119203047, which
-- stood above it and left both seeds out. None of those versions names a file now, and Migrate
-- passes over a recorded version that names none. Its next upgrade applies each list it lacks,
-- main's 20261119003157 or 20261119203047 among them, before this one, and none of them names
-- pending_basis_seed or reparent_seed, so a queued row of either stops the upgrade at the first of
-- them (SQLSTATE 23514). On such a database, with every daemon of it stopped, an operator first
-- deletes the queued rows of both kinds, with the statement of docs/kubernetes.md's rollback of
-- this release (`delete from public.outbox where kind in ('pending_basis_seed',
-- 'reparent_seed');`), then runs the upgrade, which ends with this file and admits both kinds
-- again. The delete costs little: the next boot queues a pending-basis seed again for each step
-- that still waits, and a re-parented root and its new parent's architect are not told of that one
-- move.
--
-- A daemon of an earlier release refuses neither row at the database, since this list admits
-- them, and runs neither: none of its outbox lanes lists either kind (record.LaneKinds), and its
-- sweep wakes no lane for a kind in no lane (record.Store.UnfinishedLanes), so a row of either
-- stays queued until this release runs it.
--
-- There is no down file: migrations here are forward-only.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt', 'design_gates_publish', 'update_ask',
  'merge_queue_read', 'merge_queue_removal', 'role_holders_publish', 'step_record', 'plan_record',
  'conflict_confirm', 'merge_forward', 'design_changes', 'pending_basis_seed', 'reparent_seed'));
