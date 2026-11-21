-- 20261120105818_embeddings_partitioned_mirror.up.sql — every write of embeddings, written to
-- embeddings_partitioned too, from this migration until embeddings_swap_in_partitioned drops the
-- old table (20261120105330).
--
-- The copy of the stored rows (store.FillEmbeddingProjectKeys) is committed batches that run
-- while the server serves, so a row can change after the copy has taken it, and a row can be
-- written that the copy will never reach. Each is carried over here, in the writing transaction:
-- an insert as an insert, an update as an update of the copied row, and as an insert where the
-- copy has not taken the row yet (the copy's own insert then meets it and leaves it), a delete as
-- a delete, and a truncate as a truncate. An update that changes project_key moves the copied
-- row to its new partition. The copy takes each batch's rows FOR SHARE, so no write of a row
-- runs between the copy reading it and the copy's insert committing: every write of a row lands
-- after the copy of it, and so wins.
--
-- A write of a row of a project with no partition first gives the project its partition, where
-- the project exists (embeddings_ensure_project_partition, 20261120105330): a statement can
-- write an item's row before the project it inserts beside it, and a project written with its
-- table's triggers off has none. A row of a key no project holds is refused (SQLSTATE 23514), as
-- it is on the partitioned table after the swap.
--
-- A write made as a replica (session_replication_role = replica) fires no ordinary trigger, so it
-- is not carried over. No writer of Dispatch's writes so; a restore or a replication apply into
-- this table during the copy would, and the swap then refuses wherever the two tables' counts
-- differ. Firing these triggers ALWAYS would take an ALTER TABLE of embeddings, which the census
-- does not admit over a table of its size.
--
-- What it costs. Each write of embeddings writes embeddings_partitioned too, its HNSW index
-- included, for as long as the copy runs, which is the only time both tables are there.
--
-- This migration is one create function and two create triggers, which change the catalog and
-- read no row, so the census's size limit does not apply to it however large embeddings is
-- (pgmigrate/exempt.go); CREATE TRIGGER takes SHARE ROW EXCLUSIVE on embeddings for the catalog
-- write alone. Its census answers 0.
create function embeddings_mirror_to_partitioned() returns trigger
language plpgsql as $$
begin
  if tg_op = 'TRUNCATE' then
    truncate embeddings_partitioned;
    return null;
  end if;
  if tg_op = 'DELETE' then
    if old.project_key is null then
      delete from embeddings_partitioned where project_key is null and kind = old.kind and id = old.id;
    else
      delete from embeddings_partitioned where project_key = old.project_key and kind = old.kind and id = old.id;
    end if;
    return null;
  end if;
  perform embeddings_ensure_project_partition(new.project_key);
  if tg_op = 'UPDATE' then
    if old.project_key is null then
      update embeddings_partitioned
         set (kind, id, text_snapshot, content_hash, embedded_hash, model, embedding, attempt_count,
              confirmed_failures, next_attempt_at, dead, embedded_at, created_at, pending_since,
              dead_model, covers, project_key)
           = (new.kind, new.id, new.text_snapshot, new.content_hash, new.embedded_hash, new.model,
              new.embedding, new.attempt_count, new.confirmed_failures, new.next_attempt_at, new.dead,
              new.embedded_at, new.created_at, new.pending_since, new.dead_model, new.covers, new.project_key)
       where project_key is null and kind = old.kind and id = old.id;
    else
      update embeddings_partitioned
         set (kind, id, text_snapshot, content_hash, embedded_hash, model, embedding, attempt_count,
              confirmed_failures, next_attempt_at, dead, embedded_at, created_at, pending_since,
              dead_model, covers, project_key)
           = (new.kind, new.id, new.text_snapshot, new.content_hash, new.embedded_hash, new.model,
              new.embedding, new.attempt_count, new.confirmed_failures, new.next_attempt_at, new.dead,
              new.embedded_at, new.created_at, new.pending_since, new.dead_model, new.covers, new.project_key)
       where project_key = old.project_key and kind = old.kind and id = old.id;
    end if;
    if found then
      return null;
    end if;
  end if;
  insert into embeddings_partitioned (kind, id, text_snapshot, content_hash, embedded_hash, model, embedding,
                                      attempt_count, confirmed_failures, next_attempt_at, dead, embedded_at,
                                      created_at, pending_since, dead_model, covers, project_key)
  values (new.kind, new.id, new.text_snapshot, new.content_hash, new.embedded_hash, new.model, new.embedding,
          new.attempt_count, new.confirmed_failures, new.next_attempt_at, new.dead, new.embedded_at,
          new.created_at, new.pending_since, new.dead_model, new.covers, new.project_key)
  on conflict (kind, id, project_key) do update
    set text_snapshot = excluded.text_snapshot, content_hash = excluded.content_hash,
        embedded_hash = excluded.embedded_hash, model = excluded.model, embedding = excluded.embedding,
        attempt_count = excluded.attempt_count, confirmed_failures = excluded.confirmed_failures,
        next_attempt_at = excluded.next_attempt_at, dead = excluded.dead, embedded_at = excluded.embedded_at,
        created_at = excluded.created_at, pending_since = excluded.pending_since,
        dead_model = excluded.dead_model, covers = excluded.covers;
  return null;
end;
$$;

create trigger embeddings_mirror_to_partitioned after insert or update or delete on embeddings
  for each row execute function embeddings_mirror_to_partitioned();

create trigger embeddings_mirror_truncate after truncate on embeddings
  for each statement execute function embeddings_mirror_to_partitioned();
