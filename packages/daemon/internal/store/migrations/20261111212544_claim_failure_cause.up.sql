-- 20261111212544_claim_failure_cause.up.sql — the one word for what failed a claim, beside the
-- sentence the daemon already keeps, and the cause of the episode a relaunch is under way for
-- (INFRA-3083).
--
-- claims.failure_cause is supervise.Claim.FailureCause and claims.failed_at is
-- supervise.Claim.FailedAt: the cause the site that failed the claim chose (supervise's fail) and
-- the moment it did. One sentence covers four different events today, so a reader of the sentence
-- alone cannot tell a dead process from an agent that never came up; the word is chosen by the
-- site rather than read back out of the sentence, and it is persisted so an agent reading the
-- claim after a daemon restart reads what the daemon that failed it chose. claims.pending_cause is
-- supervise.Claim.PendingCause: what failed the process whose relaunch is under way, which a
-- launch the runtime then refuses fails under, so a claim that died and whose relaunch was refused
-- reads died rather than launch_refused. Both of the first two are cleared where the sentence is,
-- by the launch that runs the claim again, and the third by the agent that comes up.
--
-- The backfill reads the sentences this release's predecessors wrote, and only those three that
-- name one event each:
--
--   'prompt retirements ran out'                                   -> prompt_retires_exhausted
--   'a provider error that cannot clear ended this claim''s turn: ' -> provider_error
--   'launch failures ran out: <the runtime's refusal>'              -> launch_refused
--
-- Each is matched on its whole leading sentence and not on a prefix of it, because two of them
-- begin alike: the colon-and-space form of the launch sentence is written by launchNow alone, over
-- a launch the runtime refused, while the bare 'launch failures ran out' is written by four
-- different events under one sentence (supervise's relaunchAfterFailure) and says which of them
-- nowhere. That bare sentence takes `unrecorded`, and so does 'deaths with work outstanding ran
-- out': it names one site, but a backfilled row may say only what the stored sentence proves of
-- the build that wrote it, and nothing distinguishes a row an older build wrote under that
-- sentence from one this release would. `unrecorded` over rows the daemon has no fact for is what
-- 0038_claim_suspended_by.up.sql does with 'unknown'.
--
-- failed_at is left null for a backfilled row: the release this upgrades from recorded the moment
-- nowhere, and a null reads as absent where an invented time would read as evidence. What is
-- served for such a claim omits the time rather than naming one.
--
-- Only a claim the store holds in 'failed' is backfilled. A claim in any other state is not
-- carrying a failure the daemon would act on, and the column it takes is the empty default, which
-- is what every claim that fails after this migration starts from.
--
-- pending_cause is empty for every row the table already holds, and nothing backfills it: the
-- release this upgrades from kept the cause of a relaunch under way in memory alone, and empty is
-- exactly what that release's own restart read. A launch the runtime refuses over such a claim
-- fails it under launch_refused, as it would have without this column.
--
-- Rolling the image back needs no restore: store's claims.go is the only reader and writer of
-- these three columns, and the release before it names them in no statement, so its upserts leave
-- whatever the columns last held and a daemon rolled forward again reads the cause back.
--
-- There is no down file: migrations here are forward-only.
alter table claims add column failure_cause text not null default '';
alter table claims add column failed_at timestamptz;
alter table claims add column pending_cause text not null default '';

update claims set failure_cause = case
  when failure like 'prompt retirements ran out%' then 'prompt_retires_exhausted'
  when failure like 'a provider error that cannot clear ended this claim''s turn:%' then 'provider_error'
  when failure like 'launch failures ran out: %' then 'launch_refused'
  else 'unrecorded'
end where state = 'failed';
