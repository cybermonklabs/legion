-- 20261128062654_embeddings_enqueue_issue_spec_latest.up.sql
-- 0140's trigger on artifact_versions enqueues an issue's embedding text for each version of its
-- primary document a statement inserts. A row trigger AFTER INSERT fires once the whole statement
-- has inserted, so every firing reads the same latest version (issue_embedding_spec), hashes the
-- same text and hands embeddings_enqueue the same row: a statement that inserts N versions of one
-- document does that work N times. The route that writes a version inserts one, so it paid once;
-- a bulk insert of many versions - dispatch-seed's COPY of a document's versions, a restore, a
-- test corpus - paid N times, each over the latest spec: 1,511 firings and 701 ms for the 200
-- issues of one seed (cmd/dispatch-seed, scale 0.01, pg_stat_statements). 20261124034909 removed
-- the same repeat from the issues-search trigger beside this one.
--
-- Now a firing enqueues the issue only when no version of the document with a higher number is
-- visible to it: in a multi-row insert that is the highest row alone, and for a single-row insert,
-- the route's, it is that row, as before. The text it enqueues is the one every firing enqueued
-- before, since every firing read the same latest version. There is no `markdown is not null`
-- here, unlike in 20261124034909's probe, because 0140's trigger has no `when`: every version
-- fires it, an image too, so the highest row of the statement, whatever it holds, is the firing
-- left to enqueue, and it reads issue_embedding_spec's latest version exactly as every firing did.
-- One shape enqueues nothing where the old body enqueued: a version inserted below one the
-- document already holds. The old body enqueued the stored latest version's text then, which
-- embeddings_enqueue skips when the row's hash is that text's, as it is whenever the latest
-- version was written through this trigger, and which repaired the row only when it was stale; no
-- writer inserts below the latest. The probe reads artifact_versions' unique (artifact_id, number)
-- index.
--
-- This replaces a function body alone and leaves the trigger as 0140 made it, so it takes no lock
-- on artifact_versions, and the census exempts it.
create or replace function embeddings_enqueue_issue_spec() returns trigger language plpgsql as $$
declare
  v_key text;
  v_title text;
begin
  select i.key, i.title into v_key, v_title
  from artifacts a join issues i on i.key = a.issue_key
  where a.id = new.artifact_id and a.is_primary;
  if found and not exists (
    select 1 from artifact_versions v
    where v.artifact_id = new.artifact_id and v.number > new.number
  ) then
    perform embeddings_enqueue('issue', v_key, issue_embedding_text(v_title, issue_embedding_spec(v_key)));
  end if;
  return new;
end;
$$;
