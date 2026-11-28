-- 20261120061840_going_plans_shown.up.sql — what a plan says and what each of its rows shows.
--
-- A run's model proposes a plan by a call of the agent's (api/going_plan.go). The server checks
-- each change against its tool spec, reads the item it changes as the person, and keeps the plan:
--
-- - going_plans.step is the step of the run whose call proposed the plan, so a second try of that
--   call answers the plan it made rather than making another; going_plans_step holds it to one
--   plan per step. Null for none.
-- - going_plans.summary is what the plan does, in the model's one or two sentences.
-- - going_plan_rows.shown is what the plan shows the person of the row's item, as the server last
--   read it when the plan was made or read: the item, the issue it is or belongs to, the fields
--   the change writes and their value now, the holder of the issue's claim, and why the row waits
--   for the person in every mode. A row's approval keeps the read it was approved over
--   (approval.seen), against which its item is read again before its write.
--
-- The tables are new on this branch (20261119201453) and empty in every deployment, so the
-- columns' defaults and the index rewrite nothing, and no earlier release names them
-- (cmd/dispatch/README.md, "Database migrations"). The identifier is the UTC moment this was
-- written, as `migrationid` requires of every migration past the numbered series.
alter table going_plans
  add column step integer,
  add column summary text not null default '';

create unique index going_plans_step on going_plans (run_id, step);

alter table going_plan_rows add column shown jsonb;
