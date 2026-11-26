-- 20261125223008_claim_held_for.up.sql — a claim held for OpenAI's cyber-policy refusal keeps the
-- cause words of the release before this one in its two cause columns (cybermonklabs/legion#5505).
--
-- This release adds the cause word `cyber_policy` (supervise.CauseCyberPolicy). The release before
-- it stores claims.failure_cause and claims.pending_cause and serves them as they stand, and every
-- plugin from before the cyber-policy contract entry reads them with a closed list of the seven
-- words before it and refuses the whole state body for an eighth. So this release never writes
-- `cyber_policy` into either column. claims.held_for holds it instead, and the claim store maps the
-- two (the claim store's storedCause, heldFor and readCause):
--
--   * Write: a cause that is `cyber_policy` in memory is written as `provider_error`, the word the
--     release before chose for the same refusal, and held_for is `cyber_policy` where
--     supervise.Claim.FailureCause or supervise.Claim.PendingCause is `cyber_policy`, else ''.
--   * Read: failure_cause and pending_cause each read as `cyber_policy` where that column holds
--     `provider_error` and held_for is `cyber_policy`, and as they stand otherwise.
--
-- The two causes share the one word. A claim whose one cause is `cyber_policy` and whose other is
-- `provider_error` would read back with both `cyber_policy`, and no path of this release makes that
-- pair: supervise's fail, the one writer of FailureCause, clears PendingCause in the same write,
-- and PendingCause takes only died, stale_plugin and never_registered (relaunchAfterFailure).
--
-- The update below moves `cyber_policy` out of the two columns of every row a build of this
-- change wrote before this column existed. No release wrote that word, so it changes no row of a
-- released daemon.
--
-- There is no down file: migrations here are forward-only. The release before this one names each
-- column of claims it reads and writes (the claim store's claimSelect and upsertClaim), so it boots
-- with held_for standing and leaves it as it is on every write; the database needs nothing taken
-- off for a rollback. That release reads a held claim as one failed under `provider_error` and
-- does not hold it: its own starts relaunch the claim and its controller keeper retries such a
-- controller. The claim's sentence still names `cyber_policy`. After the roll-forward the hold
-- comes back for each claim that release left failed or retired with `provider_error`. A claim it
-- relaunched has a cleared cause, so its stale held_for reads as nothing. A claim it relaunched and
-- then failed again under `provider_error` reads as held for `cyber_policy` until a decision
-- relaunches it, and no read can tell.
alter table claims add column held_for text not null default '';

update claims set
  held_for = 'cyber_policy',
  failure_cause = case when failure_cause = 'cyber_policy' then 'provider_error' else failure_cause end,
  pending_cause = case when pending_cause = 'cyber_policy' then 'provider_error' else pending_cause end
where failure_cause = 'cyber_policy' or pending_cause = 'cyber_policy';
