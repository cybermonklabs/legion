-- 20261116092312_reach_records_kept_with_their_acts.up.sql — three records of R3 and R5 that a
-- daemon restart must not lose: that an outbox row was held for a claim that keeps its session, the
-- launch a claim's last relaunch left it at, and the reach keeper's notices that speak for no
-- claim's reach record.
--
-- outbox.held_at is when the row was first held for a claim that keeps its session while Envoy has
-- no holder for its role (internal/daemon's heldForItsSession), written in the transaction that
-- returns the row to the outbox after that try, and never cleared. Each later try of the row asks
-- first whether that hold has ended — the claim ended, or its phase moved on — and finishes the row
-- undelivered if it has, so a holder that comes back after the phase moved is not handed it: a try
-- between that fails for some other reason, such as a read the database did not answer, leaves the
-- mark as it stands. Every existing row takes null: it was never held.
--
-- claims.relaunch_generation is supervise.Claim.LastRelaunchGeneration: the launch generation the
-- claim's last relaunch on its same session left it at, beside that relaunch's moment
-- (claims.relaunched_at). The relaunch's launch records it in the launch's own write, whichever
-- path made that launch, and a write that carries an older relaunch, or none, leaves it as it
-- stands (store's putClaim). The reach keeper
-- counts the claim's later launches from it, so a daemon that ends before its own record of the
-- relaunch is written does not take a later launch — the process exiting at its Envoy role claim
-- and launched again — for the relaunch's own. Null is a relaunch whose launch is not recorded,
-- which the keeper counts from the claim's generation as it reads it.
--
-- reach_project_notices is the record of each reach notice that speaks for no claim's reach record:
-- the notice that Envoy lists no live session (kind envoy-no-live-session, claim ''), one for the
-- project, and the lock-out of a claim's newer process (kind worker-locked-out), one for each claim,
-- with the two generations it names. told_at is when the controller's notice of it was queued,
-- written in that notice's own transaction, and null for a lock-out notice the controller's lane
-- did not queue, which the record keeps so that the next daemon tells it while its worker stream
-- still refuses the same newer process. notice is the notice as it is told and issue the issue it
-- is queued on. The no-live record is deleted when a read of the list finds a live session again,
-- which ends the run it was told of.
--
-- Rolling the image back needs no statement: the release before this one names neither column nor
-- the table, its upserts and its outbox retries leave all three as they stand, and a daemon rolled
-- forward again reads them back. A row the earlier release held for a claim carries no held_at, and
-- is published at its next try as that release would publish it.
--
-- There is no down file: migrations here are forward-only.
alter table outbox add column held_at timestamptz;

alter table claims add column relaunch_generation bigint;

create table reach_project_notices (
	project text not null,
	kind text not null,
	claim_token text not null default '',
	newer bigint not null default 0,
	older bigint not null default 0,
	issue text not null default '',
	notice jsonb not null,
	told_at timestamptz,
	primary key (project, kind, claim_token)
);
