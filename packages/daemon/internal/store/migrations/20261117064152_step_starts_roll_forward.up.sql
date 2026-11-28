-- 20261117064152_step_starts_roll_forward.up.sql — the roll forward of a rollback of the release
-- that records each step: each run of an issue that an earlier release ended gets its end, and
-- each place of an issue where it may have started a run reads 'uncounted', so no row this
-- release wrote before the rollback is taken for a run it did not count.
--
-- The count in step_starts carries a record's id from a run's start to its end, and its mark
-- decides that a run gets one end. Both hold only while this release starts and ends every run of
-- the place. An earlier release starts and ends runs and touches no row, so at the roll forward a
-- row can stand for a run that is over: an end that read it as it stands would write nothing for a
-- run whose place's last counted run had ended, or end that older record with the later run's
-- moment and result where it was still open. The rollback keeps, for each issue with a row,
-- step_run_witness of the issue (20261117004117_step_starts, which says what it reads); this
-- compares it with the witness now, for each row of the issue.
--
-- A run the row held open at the rollback is over where the earlier release did what ends it in
-- this release: the issue moved (its phase, its generation or its pass), the role that works the
-- place had its entry raised, which a start of that place does after the run before it ended, or,
-- for a step that is no merge, its root lingered or was re-admitted. A merge runs on through the
-- linger and the root's re-admission, until its merger starts again, it lands, or the linger's
-- close retires it: a close the release applied, at a generation of the root from the rollback's
-- to now's, marks its fact processed ('linger:<root>:<generation>', internal/daemon's outbox), and
-- it ends the merge of a member it did not hand over whose pull request is not open, or whose pull
-- request closed after it (workflow's lingerExpired and closed). Such a run gets its end here,
-- stopped, at the moment of the rollback, when Legion stopped recording it, as one step_record
-- row of the identity the row kept (record) — the moment the run really ended is nowhere to be
-- read, and a record left without an end would read running for good.
--
-- A place reads 'uncounted' where the earlier release may have started a run there: the role
-- that works it had its entry raised; it is accepting or the merge, whose starts can raise no
-- entry (the architect's, and a merge GitHub had made already), and the issue moved on from
-- another phase or moved back; or the issue's generation or its root's changed, whose
-- re-admission or promotion may start any place again. The daemon's boot opens the run there at
-- the next count (record.Store.OpenUncountedStep), its end ends that run, and a start counts on as before. A
-- run that still runs is no such place: nothing starts a place while its run is open. A run the
-- earlier release ended at a place it started none reads 'ended', so a late completion of that
-- run still writes its report on the record this release counted (workflow's reportEndedStep),
-- and an end with no run open there writes nothing. A run the earlier release did not end stands
-- as the one the row counted and ends under its own id. The kept witnesses go, so a later
-- rollback keeps its own.
--
-- An entry is a count of starts only while every release keeps it. A rollback to a release before
-- 20261111225552_phase_entry drops the entries, and that migration, applied again at the roll
-- forward after the rollback's moment, numbers them from 1 again; then the roll forward reads no
-- entry, and a place of a worker reads as raised where the issue moved or its root was
-- re-admitted, which is all that is left to tell.
--
-- On a database no rollback touched step_starts_rollback is empty and this changes nothing. The
-- way back below is what makes it run again: it keeps the witnesses and deletes this migration's
-- row, so the roll forward's Migrate applies this file again (store.apply decides each migration
-- on its own row). Run it with the daemon stopped, before the earlier image is pinned. A second
-- rollback before any roll forward keeps the witnesses of the first, which the earlier release has
-- moved since. docs/kubernetes.md, "Rolling back the release that records each step", carries the
-- same block, and internal/textcheck holds the two to one text.
--
-- insert into step_starts_rollback (issue, tree, witness, taken_at)
-- select key, tree, step_run_witness(key, tree), now() from issues
-- where key in (select issue from step_starts)
-- on conflict (issue) do nothing;
-- delete from schema_version where version = 20261117064152;
with witnessed as (
  select s.issue, s.task, s.kind, s.place, s.run, s.record, r.tree, r.taken_at, r.witness as was,
    step_run_witness(r.issue, r.tree) as now,
    -- The role whose entry each start of the place raises (workflow's stepOf): accepting's, the
    -- architect's, raises none.
    case s.kind when 'merge' then 'merger' when 'retro' then 'implementer'
      else (array['planner', 'implementer', 'tester', 'reviewer'])[s.place] end as worker,
    -- The phase of a place whose start may raise no entry.
    case when s.kind = 'merge' then 'merging' when s.kind = 'step' and s.place = 5 then 'accepting' end as entryless,
    not exists (select 1 from schema_version v where v.version = 20261111225552 and v.applied_at > r.taken_at) as entries_kept
  from step_starts s join step_starts_rollback r on r.issue = s.issue
), compared as (
  select w.*,
    w.was->'at' is distinct from w.now->'at' or w.was->'pass' is distinct from w.now->'pass' as moved,
    w.was->'generation' is distinct from w.now->'generation' as rerun,
    w.was->'root_generation' is distinct from w.now->'root_generation' as root_rerun,
    w.was->'lingers' is distinct from w.now->'lingers' as lingered,
    w.was->'entries'->w.worker is distinct from w.now->'entries'->w.worker as entry_raised,
    exists (
        select 1 from generate_series((w.was->>'root_generation')::bigint, (w.now->>'root_generation')::bigint) g
        join processed_events p on p.source = 'outbox' and p.event_id = 'linger:' || w.tree || ':' || g
        where not exists (select 1 from handovers h where h.issue = w.issue and h.tree = w.tree and h.linger_generation = g))
      and not exists (select 1 from pull_requests pr where pr.issue = w.issue and pr.state = 'open') as retired
  from witnessed w
), raised as (
  -- entered: the role that works the place had its entry raised, as far as the entries can tell.
  select c.*, case when c.entries_kept then c.entry_raised else c.worker is not null and (c.moved or c.root_rerun) end as entered
  from compared c
), judged as (
  -- over: what ends the run of the place in this release, where one was open; restarted: what may
  -- have started one there.
  select e.*,
    e.moved or e.rerun or e.entered or case when e.kind = 'merge' then e.retired else e.root_rerun or e.lingered end as over,
    e.rerun or e.root_rerun or e.entered
      or e.entryless is not null and e.moved
        and not (e.was->>'at' is not distinct from e.entryless and e.was->'pass' is not distinct from e.now->'pass') as restarted
  from raised e
), decided as (
  select j.*, j.run = 'open' and j.over as ended, (j.run <> 'open' or j.over) and j.restarted as uncounted from judged j
), ends as (
  insert into outbox (kind, issue, payload, attempts, next_at, last_error)
  select 'step_record', issue, record || jsonb_build_object('ended_at', taken_at, 'result', 'stopped'), 0, now(), ''
  from decided where ended and record is not null
)
update step_starts s set run = case when d.uncounted then 'uncounted' else 'ended' end
from decided d
where s.issue = d.issue and s.task = d.task and s.kind = d.kind and s.place = d.place
  and s.run <> 'uncounted' and (d.uncounted or d.ended);

delete from step_starts_rollback;
