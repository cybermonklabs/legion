-- 20261115083448_claim_suspended_by_architect.up.sql — 'architect' joins the sources of
-- supervise.Claim.SuspendedBy: an architect suspended a claim of its own tree through its own
-- `legion` tool (api's claimSuspend). The daemon holds it as it holds the controller's
-- suspension: no rule resumes it, it keeps the claim's phase and its pending delivery, and only a
-- resume by an architect, the controller or the operator lifts it. The check 20261115060739 wrote
-- is dropped and written again with the new value. No row holds it yet, so there is nothing to
-- backfill.
alter table claims drop constraint if exists claims_suspended_by_check;
alter table claims add constraint claims_suspended_by_check
  check (suspended_by in ('', 'unknown', 'operator', 'daemon', 'exit', 'released', 'controller', 'architect'));
