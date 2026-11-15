-- 20261110225246_backward_moves_and_cap_told.up.sql — the daemon counts every backward move of an
-- issue by its kind, tells the review_round_cap message once for each value of the cap, and puts a
-- hook's prompt in front of a role through the outbox (INFRA-2551).
--
-- backward_moves is one count for each kind of move, keyed "<from>-<to>" with the phase words of
-- the daemon's forward line, "testing-implementing" and "reviewing-implementing" among them
-- (internal/phase's BackwardPairs). It is counted on the implementer's row, beside rounds, and a
-- hook reads it to decide when to prompt the issue's roles. rounds keeps its meaning exactly and
-- is not replaced: a phase completion's idempotency key is built from it, so it rises on every
-- backward move, the move the daemon makes for a head that no longer merges into its base
-- included. That move raises rounds and conflict_rounds and no pair, because nobody sent the work
-- back.
--
-- What a reader of the production table must know: the counts start empty here and are never
-- filled in from the past. The daemon has no record of which kind each backward move behind an
-- issue was, so this migration writes no count for one. An issue already in flight therefore
-- holds a map whose counts add up to less than its rounds, and reads so for the rest of its run.
-- Only a move made after the deploy is counted by its kind.
--
-- cap_told is the review_round_cap value the row last posted the cap's message and its pr-blocked
-- notice for, 0 for a row that has posted none. Before this column the daemon posted both on
-- every counted round at or above the cap, which woke the architect again for news it already
-- had. Every row starts at 0, which is the value of no cap, so the first counted round at or
-- above the cap after the deploy tells once and the rounds behind it tell nothing. A new
-- generation sets it back to 0 with rounds (record's clearGeneration), so a tree sent back to the
-- start tells again from its own first crossing.
--
-- The kind check is one list, so the migration that sets it names every kind, as 0049,
-- 20261101222956, 20261106041346 and 20261108000545 did: whichever of two open branches merges
-- last admits both their kinds. The kind this one adds is hook_prompt, the row that carries a
-- hook's prompt to one role of the issue.
--
-- There is no down file: migrations here are forward-only.
alter table phases add column backward_moves jsonb not null default '{}'::jsonb;
alter table phases add column cap_told integer not null default 0;

alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_wake', 'check_look', 'check_bound',
  'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish', 'claim_release',
  'hook_prompt'));
