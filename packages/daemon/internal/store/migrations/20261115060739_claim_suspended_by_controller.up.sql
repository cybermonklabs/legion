-- 20261115060739_claim_suspended_by_controller.up.sql — 'controller' joins the sources of
-- supervise.Claim.SuspendedBy: the project's controller suspended the claim's agent through its own
-- `legion` tool (api's suspendAgent). The daemon holds it as it holds an operator's suspension:
-- no rule resumes it, it keeps the claim's phase and its pending delivery, and only a resume by
-- the controller or the operator lifts it. The check 20261101201850 wrote is dropped and written
-- again with the new value. No row holds it yet, so there is nothing to backfill.
alter table claims drop constraint if exists claims_suspended_by_check;
alter table claims add constraint claims_suspended_by_check
  check (suspended_by in ('', 'unknown', 'operator', 'daemon', 'exit', 'released', 'controller'));
