-- 20261124034909_artifact_versions_issues_search_latest.up.sql
-- 20261105145243's trigger re-vectors an issue for each version of its primary document a
-- statement inserts. A row trigger AFTER INSERT fires once the whole statement has inserted, so
-- every firing reads the same latest version (issue_embedding_spec) and writes the same vector: a
-- statement that inserts N versions of one document builds that vector N times. The route that
-- writes a version inserts one, so it paid once; a bulk insert of many versions - dispatch-seed's
-- COPY of a document's versions, a restore, a test corpus - paid N times, each over the latest
-- spec, which at 100-660 KB took about 100 ms a build: 28 builds of one large document were 2.6 s
-- of a seed's 25 s documents step (cmd/dispatch-seed, scale 0.01, pg_stat_statements).
--
-- Now a firing re-vectors the issue only when no version of the document with a higher number and
-- with markdown is visible to it: in a multi-row insert that is the highest such row alone, and for
-- a single-row insert, the route's, it is that row, as before. The vector it writes is the one the
-- last firing wrote before, since every firing read the same latest version. One shape writes
-- nothing where the old body wrote: a version inserted below one the document already holds.
-- The old body rewrote the vector of the stored latest version then, which changed nothing when
-- that vector was current and repaired it when it was stale; no writer inserts below the latest,
-- and the vector is current whenever the latest version was written through this trigger. The `markdown is not
-- null` matches the trigger's own `when`: a higher version with no markdown (an image) fires no
-- trigger, so it must not stop the highest version that does fire from re-vectoring, which then
-- reads issue_embedding_spec's latest version exactly as before. The probe reads
-- artifact_versions' unique (artifact_id, number) index.
--
-- A statement-level trigger with a transition table re-vectors each issue once too and measured
-- the same (39-47 s against 39-42 s for this, 67-72 s before, two seeds a run, three runs on each
-- of two nodes), but replacing a row trigger with one takes a lock on artifact_versions; this is
-- a function body alone, so it takes none, and the census exempts it.
create or replace function artifact_versions_issues_search() returns trigger language plpgsql as $$
declare
  v_key text;
begin
  select i.key into v_key
  from artifacts a join issues i on i.key = a.issue_key
  where a.id = new.artifact_id and a.is_primary;
  if found and not exists (
    select 1 from artifact_versions v
    where v.artifact_id = new.artifact_id and v.number > new.number and v.markdown is not null
  ) then
    update issues set search = issue_search_vector(key, title, issue_embedding_spec(key))
     where key = v_key;
  end if;
  return null;
end $$;
