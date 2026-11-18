-- 20261117004117_step_starts.up.sql — the count of starts of each step of an issue, which carries a
-- step record's id from the step's start to its end (internal/workflow's StepID), and what a
-- rollback keeps of each issue for the roll forward to compare.
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
-- run is where the latest run of the place stands (record.StepRun). 'open': the latest start's
-- run has no end yet, and the next end of the step running there is its end. 'ended': that run has
-- its end, written in the transaction that set the mark, and a later end of it writes nothing:
-- the settle of a fact is the one writer of an end (workflow's settleRuns), and the mark holds it
-- to one end of a run whichever later fact asks again, so Dispatch never takes a later end of a
-- run as its end. 'uncounted': the run the row counted is over, and a run the place holds now
-- started under an earlier release, which counts no start: its end is a record of its own at the
-- next count. Only the roll forward of a rollback writes it
-- (20261117064152_step_starts_roll_forward).
-- The next start of the place sets 'open' whatever stood before, and so does the daemon's boot
-- for the run an 'uncounted' row stands for (record.Store.OpenUncountedStep). record is the
-- identity of the latest start's record — the fields each write of it carries, its id among them,
-- as the start wrote them (record.Store.RaiseStepStarts) — kept for the roll forward of a
-- rollback, which writes that record's end where an earlier release ended its run; null on a row
-- only an end of an uncounted run wrote, which is ended.
--
-- The count is keyed by task (the daemon writes the one task, default, of today's fixed phases), and a
-- re-admission does not clear it (record.Store.ClearGeneration keeps it): the generation is part of
-- the id already, and the count only rises. A step running when this release deploys has no row:
-- the daemon's boot writes one, open, at the count 0, with the agent its start would have named and
-- its moment where its role's phase entry records one (null where none does: accepting's), and its
-- end then reads that row as it reads a start's (workflow's OpenUncountedRuns).
--
-- An earlier release reads and writes none of this, so after a rollback it moves issues, and starts
-- and ends their steps, with each row standing as this release left it. step_starts_rollback is
-- what the rollback keeps of each issue with a row, run with the daemon stopped
-- (docs/kubernetes.md, "Rolling back the release that records each step"): the issue's tree,
-- step_run_witness of the issue then, and the moment of the rollback. At the roll forward
-- 20261117064152 reads the witness again and compares the two for each row of the issue: a run
-- open at the rollback that the earlier release ended gets its end, stopped at that moment, and a
-- place where it may have started a run this release did not count reads 'uncounted' from then on.
--
-- step_run_witness is what this release's own model ends or starts a run on, read from the
-- columns every earlier release writes as it does those things, as one object:
--   * at: the phase the issue stands in, the phase its hold is from where it is held
--     (record.StandsAt): a transition, a leave and a merge's close move it;
--   * generation: the issue's, which a re-admission, a child's re-entry into its tree and an
--     adoption each raise;
--   * pass: the implementer's, which every move back raises: an issue that left a phase and came
--     back to it stands where it stood, with its pass raised;
--   * entries: the entry of each role of the issue (phases.entry), which each start that hands a
--     phase worker its work raises (workflow's enter): a move into the phase, and the promotion
--     of a tree admitted again, which starts a member's merger again and ends the merge the
--     linger left open (admit's startMidPhaseMembers);
--   * root_generation: the generation of the tree's root, which the root's re-admission raises:
--     a member's step the linger ended starts again at its promotion, with the member's own
--     generation unchanged, and a member's merge the linger left open runs on until its merger
--     starts again;
--   * lingers: whether the root lingers, whose start ends every member's step but a merge
--     awaiting its pull request (workflow's beginLinger).
-- The tree is the one the rollback found the issue in, so a hand-over, which keeps the record of a
-- merge it hands over open until the re-admission ends it, changes nothing here. An issue with no
-- row in issues reads null, and a null witness against a null one reads as unchanged.
--
-- The table refuses a row no step has, as the outbox refuses a kind it does not name: a task
-- named, a kind of step, retro or merge, a step at its place from 1 and a retro or a merge at 0
-- (workflow's stepOf, and the rule Dispatch's route holds a record's place to), and a count that
-- starts at 0.
create table step_starts (
  issue text not null,
  task text not null check (task <> ''),
  kind text not null check (kind in ('step', 'retro', 'merge')),
  place integer not null,
  starts bigint not null check (starts >= 0),
  agent text not null,
  started_at timestamptz,
  run text not null check (run in ('open', 'ended', 'uncounted')),
  record jsonb,
  primary key (issue, task, kind, place),
  check (case when kind = 'step' then place >= 1 else place = 0 end)
);

-- An agent's request asks whether the record names that agent the agent of a run that is not
-- ended, on any issue (record.RunsAStep), and no row is ever deleted: this index holds the agent
-- of each run not ended alone, so the read is not a scan of every row ever written.
create index step_starts_running_agent on step_starts (agent) where run <> 'ended';

create table step_starts_rollback (
  issue text primary key,
  tree text not null,
  witness jsonb,
  taken_at timestamptz not null
);

create function step_run_witness(issue_key text, tree_key text) returns jsonb language sql stable as $$
  select jsonb_build_object(
    'at', coalesce(case when i.phase = 'held' then i.held_from end, i.phase),
    'generation', i.generation,
    'pass', (select p.pass from phases p where p.issue = i.key and p.role = 'implementer'),
    'entries', (select jsonb_object_agg(p.role, p.entry) from phases p where p.issue = i.key),
    'root_generation', root.generation,
    'lingers', root.linger_until is not null)
  from issues i left join issues root on root.key = tree_key
  where i.key = issue_key
$$;
