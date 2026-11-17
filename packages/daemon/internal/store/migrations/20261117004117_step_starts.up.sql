-- 20261117004117_step_starts.up.sql — the count of starts of each step of an issue, which carries a
-- step record's id from the step's start to its end (internal/workflow's StepID).
--
-- A record's id is a uuid of the issue, its run, the task, the kind, the place and this count. The
-- start of a step raises the count by one in the transaction that writes the start, and the write
-- that ends the step reads it unchanged, maybe in another transaction of another daemon after a
-- restart: no start of the same place comes between the two, because the step runs until one of
-- the ends ends it. agent and started_at are what that latest start wrote, which the end writes
-- again, so the two writes agree whichever reaches Dispatch first: a merge GitHub already made
-- starts no merger and names no agent, and a start that raised no phase entry has no other
-- record of its moment.
--
-- ended is the mark that the latest start's run has ended, set in the transaction that writes
-- that end and cleared by the next start. One run gets one end: two writes can each end the same
-- run, as a tree's linger and the hand-over at its close both end a member's step, and the second
-- finds the mark and writes nothing, so Dispatch never takes the later end of a run as its end.
--
-- The count is keyed by task (slice 1 writes the one task, default, of today's fixed phases), and a
-- re-admission does not clear it (record.Store.ClearGeneration keeps it): the generation is part of
-- the id already, and the count only rises. A step running when this release deploys has no row:
-- its end writes one at the count 0, marked ended, with the agent its start would have named and
-- its moment where its role's phase entry records one (null where none does: accepting's). Nothing
-- of an earlier release reads or writes the table, so a rollback leaves it in place.
create table step_starts (
  issue text not null,
  task text not null,
  kind text not null,
  place integer not null,
  starts bigint not null,
  agent text not null,
  started_at timestamptz,
  ended boolean not null,
  primary key (issue, task, kind, place)
);
