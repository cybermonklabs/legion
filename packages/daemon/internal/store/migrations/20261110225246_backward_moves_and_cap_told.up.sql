-- 20261110225246_backward_moves_and_cap_told.up.sql — the daemon counts every backward move of an
-- issue by its kind, tells the review_round_cap message once for each value of the cap, and puts a
-- hook's prompt in front of a role through the outbox (INFRA-2551).
--
-- backward_moves is one count for each kind of move, keyed "<from>-<to>" with the phase words of
-- the daemon's forward line, "testing-implementing" and "reviewing-implementing" among them
-- (internal/phase's BackwardPairs). It is counted on the implementer's row, beside pass, and a
-- hook reads it to decide when to prompt the issue's roles. pass is what rises on every backward
-- move, since a phase completion's idempotency key is built from it, and rounds is the review's
-- own count, which only a review that requested changes raises and review_round_cap bounds
-- (internal/workflow's recordPass and recordRound). The move the daemon makes for a head that no
-- longer merges into its base raises the pass, raises rounds and conflict_rounds where a review
-- opened the round, and raises no pair at all, because nobody sent the work back.
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
-- last admits both their kinds. This one is that last word: hook_prompt, the row that carries a
-- hook's prompt to one role of the issue, beside this release's own eighteen from
-- 20261108152000 — role_message among them, and the three merge-lane kinds
-- (merge_queue_publish, auto_merge_disable, ready_merge) left out, since the merger merges the
-- head it read and the daemon arms no auto-merge (dispatch://INFRA-714). It drops the constraint
-- if it exists before adding it, so it is safe on a database ending at either branch.
--
-- There is no down file: migrations here are forward-only.
alter table phases add column backward_moves jsonb not null default '{}'::jsonb;
alter table phases add column cap_told integer not null default 0;

alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release',
  'gated_steps_publish', 'claim_release', 'hook_prompt'));
