-- 20261124073512_claim_cause_times.up.sql — when the daemon recorded a claim's last death cause
-- and its last start failure, and the digest of the cause each time dates (#5349).
--
-- claims.last_death_at is supervise.Claim.LastDeathAt and claims.last_start_failure_at is
-- supervise.Claim.LastStartFailureAt: the time, by the daemon's clock, at which it recorded the
-- cause in claims.last_death_cause and in claims.last_start_failure, null where that cause is
-- empty. claims.last_death_digest and claims.last_start_failure_digest are the 64-bit FNV-1a of
-- the cause text each time dates, as the row stores it, in 8 bytes, null where the time is: the
-- claim store's upsertClaim computes each from the claim's cause and time in the statement that
-- writes them (causeDigest). It tells whether the time is still the cause's and protects nothing.
-- Each time is set and cleared only together with its cause (supervise.Claim.LastDeathAt states
-- the rule), so a cause recorded again with the same text is given its new time. A claim keeps
-- its death's cause through every later ready, and a reader of the state took a cause of days
-- before for the current one, since nothing in the row dated it; the claim view and the
-- operator's claim view serve both times beside their causes (lastDeathAt and
-- lastStartFailureAt). No surface serves a digest.
--
-- It back-fills nothing. Every row written before it reads null for all four: a cause that row
-- holds was recorded at a time no release wrote down, and a time made up here would be read as
-- the daemon's own. Such a cause is served with no time, as before, until the daemon records
-- another of its kind.
--
-- There is no down file: migrations here are forward-only. The release before this one names each
-- column of claims it reads and writes (the claim store's claimSelect and upsertClaim), so it
-- boots with the four columns standing, reads none of them, and leaves each as it is on every
-- write; the database needs nothing taken off for a rollback. What the rollback costs is the
-- times themselves, since that release rewrites a cause and leaves the time and the digest of
-- the cause before it. This release reads a time back only while its digest is the digest of the
-- cause the row holds, and a start failure's time before last_ready as none, since it drops the
-- start failure with its time at every ready (the claim store's scanClaim). So after the
-- roll-forward a cause that release wrote with other text is served with no time, and so is a
-- start failure it wrote after a ready. A cause it wrote with the same text, a death or a start
-- failure with no ready between, is served with the older cause's time until this release
-- records the next cause of that kind, and no read can tell.
alter table claims add column last_death_at timestamptz;
alter table claims add column last_start_failure_at timestamptz;
alter table claims add column last_death_digest bytea;
alter table claims add column last_start_failure_digest bytea;
