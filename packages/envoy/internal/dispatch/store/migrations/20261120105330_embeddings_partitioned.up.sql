-- 20261120105330_embeddings_partitioned.up.sql — embeddings again, list-partitioned by the
-- project its row belongs to, built beside the table it replaces, so each project's vectors are
-- one HNSW graph of their own.
--
-- Why. Meaning search walks one HNSW graph over every vector (embeddings_cosine, 0072) and keeps
-- the rows its reader may see as it walks (api/search.go, searchQueryMeaningBody). Once rows the
-- reader may not see sit nearer the query than the rows they may, the walk never reaches the
-- latter: through GET /api/v1/search a reader's 33 hits fell to 0 with 100 nearer vectors of a
-- private project, and raising hnsw.ef_search, hnsw.max_scan_tuples or
-- hnsw.scan_mem_multiplier changed nothing (api/hnsw_hidden_vectors_test.go). A graph per project
-- holds no vector of another project, so a search that reads only the partitions of the projects
-- its reader sees walks graphs that hold nothing hidden from them by project.
--
-- The shape. embeddings_partitioned has embeddings' columns, defaults and checks, and an index on
-- the parent for each of embeddings' indexes, so every partition carries all of them; the HNSW
-- index is one of them, so each partition is one graph. The partition key is project_key: a
-- partition per project (embeddings_add_project_partition, below), and one for the rows of no
-- project (embeddings_no_project), which holds the agent conversations' windows and any row whose
-- owner names no project. There is no DEFAULT partition: a row of a project with no partition is
-- refused with SQLSTATE 23514, and every project has one - every project that exists now from the
-- select below, and every later one from the trigger on projects, in the transaction that inserts
-- it.
--
-- The key. A unique constraint of a partitioned table must hold its partition key, so (kind, id),
-- embeddings' primary key, is (kind, id, project_key) here, NULLS NOT DISTINCT so the no-project
-- partition holds one row per (kind, id) as well. One row per (kind, id) across partitions is
-- the writers' to keep: a row's project is its owner's and never changes, and the one move there
-- is, a row stored before its project was known going from the no-project partition to its
-- project's, is an UPDATE (embeddings_enqueue and embeddings_enqueue_window after the swap,
-- store.FillEmbeddingProjectKeys, and embeddings_label_project_key_after_insert).
--
-- Beside and swap, rather than in place. Production's embeddings is 12.7 GiB (the pre-deploy
-- census of 2026-11-12) of about a million rows. Rewritten in a migration it is one transaction
-- that copies every row and builds every partition's indexes while holding writes off, and the
-- census refuses any migration that rewrites or renames a table above
-- pgmigrate.CensusTableLimit (1 GiB). So this migration creates the new table empty, reads
-- embeddings not at all and locks no existing table above ACCESS SHARE but projects, whose trigger
-- it creates: its census answers 0 whatever embeddings holds.
-- 20261120105818 mirrors every later write of embeddings into it; store.FillEmbeddingProjectKeys,
-- the leased pass the boot already starts (api.RunEmbeddingProjectKeyFill), copies the stored
-- rows in committed batches and then, once the fleet holds no build of another commit
-- (store.ErrEmbeddingsSwapWaits: server_processes within the registry's prune age, and
-- dashboard_publishes, 20261121120653, within the hour), calls embeddings_swap_in_partitioned, which drops
-- the old table and renames this one in one short transaction. Until the swap every reader reads
-- the old table, by the same name, as before.
create table embeddings_partitioned (
  kind text not null,
  id text not null,
  text_snapshot text not null,
  content_hash text not null,
  embedded_hash text,
  model text,
  embedding vector(1536),
  attempt_count integer not null default 0,
  confirmed_failures integer not null default 0,
  next_attempt_at timestamptz not null default now(),
  dead boolean not null default false,
  embedded_at timestamptz,
  created_at timestamptz not null default now(),
  pending_since timestamptz not null default now(),
  dead_model text,
  covers text[],
  project_key text,
  constraint embeddings_partitioned_kind_check check (
    kind in ('issue', 'document', 'comment', 'ask', 'message',
             'document_block', 'comment_block', 'message_block', 'ask_part')
  ),
  constraint embeddings_partitioned_key unique nulls not distinct (kind, id, project_key)
) partition by list (project_key);

-- embeddings' own indexes, each on the parent so that every partition has its own. The names
-- lose their _partitioned at the swap; a partition's index is named embeddings_<name>_<suffix>
-- from the start (embeddings_add_project_partition), so the swap renames no partition's index.
create index embeddings_partitioned_cosine on embeddings_partitioned
  using hnsw (embedding vector_cosine_ops) where embedding is not null;
create index embeddings_partitioned_kind_project on embeddings_partitioned (kind, project_key)
  where kind in ('issue', 'document_block', 'comment_block', 'message_block', 'ask_part');
create index embeddings_partitioned_pending on embeddings_partitioned (next_attempt_at, kind, id)
  where embedded_hash is distinct from content_hash and not dead;
create index embeddings_partitioned_window_embedded on embeddings_partitioned (kind)
  where kind ~ '^(document_block|comment_block|message_block|ask_part)$' and text_snapshot ~ '[^[:space:]]'
    and embedding is not null and embedded_hash = content_hash;
create index embeddings_partitioned_window_owner on embeddings_partitioned (kind, split_part(id, '#', 1))
  where kind in ('document_block', 'comment_block', 'message_block', 'ask_part');
create index embeddings_partitioned_window_unembedded on embeddings_partitioned (kind)
  where kind ~ '^(document_block|comment_block|message_block|ask_part)$' and text_snapshot ~ '[^[:space:]]'
    and (embedding is null or embedded_hash is distinct from content_hash);

-- embeddings_name_partition_indexes names every index of partition after the parent's index it
-- belongs to, embeddings_<parent's name less embeddings_ or embeddings_partitioned_>_<suffix>, so
-- a plan's index scan of a partition says which index and which partition it reads.
create function embeddings_name_partition_indexes(p_partition regclass, p_suffix text) returns void
language plpgsql as $$
declare
  v_index record;
begin
  for v_index in
    select child.oid::regclass as child,
           regexp_replace(parent.relname, '^embeddings_(partitioned_)?', '') as role
      from pg_index x
      join pg_class child on child.oid = x.indexrelid
      join pg_inherits link on link.inhrelid = child.oid
      join pg_class parent on parent.oid = link.inhparent
     where x.indrelid = p_partition
  loop
    execute format('alter index %s rename to %I', v_index.child, 'embeddings_' || v_index.role || '_' || p_suffix);
  end loop;
end;
$$;

-- embeddings_partitions_parent is the partitioned table of embeddings: embeddings_partitioned
-- until embeddings_swap_in_partitioned renames it, embeddings after. Both names are read with
-- to_regclass, which answers null for a name no relation holds, since a regclass literal is read
-- when the function is, and one of the two names is always gone.
create function embeddings_partitions_parent() returns regclass
language sql stable as $$
  select case when (select c.relkind from pg_class c where c.oid = to_regclass('embeddings')) = 'p'
              then to_regclass('embeddings') else to_regclass('embeddings_partitioned') end
$$;

-- embeddings_add_project_partition gives project p_key its partition, embeddings_project_<key in
-- lower case> (a key is an upper-case letter then upper-case letters or digits, the projects.key
-- check, so no two keys share a name), and does nothing when it has one. It attaches a table
-- made empty rather than creating the partition in place: CREATE TABLE ... PARTITION OF takes
-- ACCESS EXCLUSIVE on the parent, which queues every search behind the transaction that creates
-- a project, where ATTACH PARTITION takes SHARE UPDATE EXCLUSIVE, which no read or write of the
-- table waits on. The table attached is empty, so the attach scans nothing and builds each index
-- over no row.
--
-- The parent is named in its statements, and the swap renames it: a swap that commits after the
-- parent is read and before those statements run leaves them naming a table that is gone
-- (SQLSTATE 42P01), which is the first project of a server whose boot swapped an empty table in
-- as the project was written. So the parent is first locked by the name read, in the mode the
-- attach takes anyway, which the swap's ACCESS EXCLUSIVE excludes. A name the swap took away is
-- refused by the lock (42P01) and read again, and once the lock is held the parent cannot be
-- renamed until this transaction ends.
create function embeddings_add_project_partition(p_key text) returns void
language plpgsql as $$
declare
  v_parent regclass;
  v_partition text := 'embeddings_project_' || lower(p_key);
begin
  if to_regclass(v_partition) is not null then
    return;
  end if;
  loop
    v_parent := embeddings_partitions_parent();
    begin
      execute format('lock table %s in share update exclusive mode', v_parent);
    exception when undefined_table then
      continue;
    end;
    exit when embeddings_partitions_parent() = v_parent;
  end loop;
  if to_regclass(v_partition) is not null then
    return;
  end if;
  execute format('create table %I (like %s including defaults including constraints including storage)', v_partition, v_parent);
  execute format('alter table %s attach partition %I for values in (%L)', v_parent, v_partition, p_key);
  perform embeddings_name_partition_indexes(v_partition::regclass, lower(p_key));
end;
$$;

create table embeddings_no_project partition of embeddings_partitioned for values in (null);
select embeddings_name_partition_indexes('embeddings_no_project'::regclass, 'no_project');
select embeddings_add_project_partition(key) from projects order by key;

-- A project inserted from here on gets its partition in the inserting transaction, whichever
-- path inserts it: POST /api/v1/projects (api/projects.go) and every seed and test that writes
-- projects alike. The trigger fires ALWAYS, so a session that writes as a replica
-- (session_replication_role = replica, as a logical-replication apply or a restore does), whose
-- writes fire no ordinary trigger, still gives each project its partition.
create function projects_embeddings_partition() returns trigger
language plpgsql as $$
begin
  perform embeddings_add_project_partition(new.key);
  return null;
end;
$$;

create trigger projects_embeddings_partition after insert on projects
  for each row execute function projects_embeddings_partition();
alter table projects enable always trigger projects_embeddings_partition;

-- embeddings_ensure_project_partition gives project p_key its partition before a row of it is
-- written, where the project exists and the partition does not: a statement that inserts a
-- project and an item of it at once can run the item's embeddings write first (a WITH query a
-- statement does not read runs last), and a project inserted with its table's triggers off has
-- none. A key no project holds gets no partition, so its row is refused (SQLSTATE 23514), as it
-- must be. The writers that run outside a statement on the table they attach to call it:
-- embeddings_mirror_to_partitioned, which attaches to embeddings_partitioned from a write of
-- embeddings, and the enqueue functions the swap installs, which run from the owners' triggers.
-- The label trigger cannot (embeddings_label_project_key_after_insert); the copy calls it for
-- every project before it begins (store.partitionEmbeddings). It costs one catalog lookup where
-- the partition is there.
create function embeddings_ensure_project_partition(p_key text) returns void
language plpgsql as $$
begin
  if p_key is not null and to_regclass('embeddings_project_' || lower(p_key)) is null
     and exists (select 1 from projects where key = p_key) then
    perform embeddings_add_project_partition(p_key);
  end if;
end;
$$;

-- embeddings_reader_partitions is the partitions a meaning search of the reader whose
-- visibility argument is viewer (refs.VisibilityArg) reads, as the project keys of those
-- partitions: null for every partition, which is the server's own viewer and an open viewer of a
-- deployment holding no private project (both see every project), and otherwise the projects the
-- viewer sees. The no-project partition is read by every reader and is not in the answer
-- (api/search.go, searchReaderPartitions). It is immutable, which it is - it reads its argument
-- alone - so a statement planned with the viewer bound (plan_cache_mode = force_custom_plan,
-- api/search.go's searchMeaningScan) folds it to a constant and prunes the partitions the reader
-- does not see from the plan.
create function embeddings_reader_partitions(viewer jsonb) returns text[]
language sql immutable parallel safe as $$
  select case
    when coalesce((viewer->>'all')::boolean, false) then null
    when not coalesce((viewer->>'private')::boolean, true) and coalesce((viewer->>'open')::boolean, false) then null
    else coalesce(array(select jsonb_array_elements_text(viewer->'sees')), '{}')
  end
$$;

-- The copy's progress: the keyset cursor over embeddings' primary key and whether every stored
-- row has been copied (store.FillEmbeddingProjectKeys), one row, as
-- embeddings_project_key_backfill (20261109120100) is for the labelling.
create table embeddings_partitioned_copy (
  singleton boolean primary key default true check (singleton),
  kind text not null default '',
  id text not null default '',
  done boolean not null default false
);
insert into embeddings_partitioned_copy default values;

-- embeddings_label_project_key_after_insert is 20261110064913's label, for the partitioned
-- table: a row inserted with no project takes its owner's. A BEFORE trigger cannot do it there,
-- since the row is routed to the no-project partition before it runs and Postgres refuses a
-- BEFORE trigger that moves a row to another partition, so this one moves the row by an UPDATE
-- once it is in. The swap installs it.
--
-- It runs inside a statement that writes embeddings, where Postgres refuses an ATTACH PARTITION
-- of embeddings (SQLSTATE 55006), so it cannot give a project its partition as the other writers
-- do (embeddings_ensure_project_partition). A project without one is a project written with its
-- table's triggers off, since the trigger on projects fires ALWAYS; the label then refuses the row
-- with SQLSTATE 23514, as a row of a project with no partition is refused anywhere, and says how
-- to give the project its partition.
create function embeddings_label_project_key_after_insert() returns trigger
language plpgsql as $$
declare
  v_project text;
begin
  if new.project_key is null then
    v_project := embeddings_owner_project_key(new.kind, new.id);
    if v_project is not null then
      if to_regclass('embeddings_project_' || lower(v_project)) is null then
        raise exception 'project % has no partition of embeddings, so % % cannot be stored under it', v_project, new.kind, new.id
          using errcode = '23514',
                hint = format('Give it one: select embeddings_add_project_partition(%L);', v_project);
      end if;
      update embeddings set project_key = v_project
       where kind = new.kind and id = new.id and project_key is null;
    end if;
  end if;
  return null;
end;
$$;

-- embeddings_swap_in_partitioned replaces embeddings with embeddings_partitioned once the copy
-- has carried every stored row over (store.FillEmbeddingProjectKeys calls it, then, and only
-- then), in one transaction under both tables' ACCESS EXCLUSIVE locks, which the caller bounds by
-- its lock_timeout: it drops the old table, with its triggers, the mirror's function and the
-- copy's progress row, renames the new table and its indexes to the old names, installs the
-- label trigger above, and points the two enqueue functions at the new key. It refuses, changing
-- nothing, when the two tables do not hold the same number of rows, and does nothing when
-- embeddings is partitioned already. The caller counts both tables without these locks first
-- and empties and copies again on a difference (store.PartitionEmbeddings), so this count under
-- the locks refuses only a write that bypassed the mirror in the moment between the two.
create function embeddings_swap_in_partitioned() returns void
language plpgsql as $$
declare
  v_stored bigint;
  v_copied bigint;
begin
  if embeddings_partitions_parent() = 'embeddings'::regclass then
    return;
  end if;
  lock table embeddings, embeddings_partitioned in access exclusive mode;
  select count(*) into v_stored from embeddings;
  select count(*) into v_copied from embeddings_partitioned;
  if v_stored <> v_copied then
    raise exception 'embeddings holds % rows and embeddings_partitioned % rows: the copy is not complete', v_stored, v_copied
      using errcode = 'P0001',
            hint = 'A write reached one table and not the other (made as a replica, with the mirror''s trigger disabled, or by hand); the fill counts both again on its next try and empties and copies the copy again where they still differ.';
  end if;

  drop table embeddings;
  drop function embeddings_mirror_to_partitioned();
  drop function embeddings_label_project_key();
  drop table embeddings_partitioned_copy;
  alter table embeddings_partitioned rename to embeddings;
  alter table embeddings rename constraint embeddings_partitioned_kind_check to embeddings_kind_check;
  alter table embeddings rename constraint embeddings_partitioned_key to embeddings_key;
  alter index embeddings_partitioned_cosine rename to embeddings_cosine;
  alter index embeddings_partitioned_kind_project rename to embeddings_kind_project;
  alter index embeddings_partitioned_pending rename to embeddings_pending;
  alter index embeddings_partitioned_window_embedded rename to embeddings_window_embedded;
  alter index embeddings_partitioned_window_owner rename to embeddings_window_owner;
  alter index embeddings_partitioned_window_unembedded rename to embeddings_window_unembedded;

  create trigger embeddings_project_key_label after insert on embeddings
    for each row execute function embeddings_label_project_key_after_insert();

  -- 20261109120000's two write functions, on the partitioned key. A row stored before its
  -- project was known is in the no-project partition, and the insert below would add a second
  -- row beside it under the project's key rather than meet it, so it is moved first; it is the
  -- one row of the key a write can meet in another partition, since a row's project is its
  -- owner's and never changes.
  create or replace function embeddings_enqueue(p_kind text, p_id text, p_text text) returns void
  language plpgsql as $enqueue$
  declare
    v_hash text := encode(sha256(convert_to(p_text, 'UTF8')), 'hex');
    v_project text := embeddings_owner_project_key(p_kind, p_id);
  begin
    perform embeddings_ensure_project_partition(v_project);
    if v_project is not null then
      update embeddings set project_key = v_project where kind = p_kind and id = p_id and project_key is null;
    end if;
    insert into embeddings (kind, id, text_snapshot, content_hash, project_key)
    values (p_kind, p_id, p_text, v_hash, v_project)
    on conflict (kind, id, project_key) do update
      set text_snapshot = excluded.text_snapshot,
          content_hash = excluded.content_hash,
          attempt_count = 0,
          confirmed_failures = 0,
          next_attempt_at = now(),
          dead = false,
          dead_model = null,
          pending_since = now()
      where embeddings.content_hash is distinct from excluded.content_hash;
    if found then
      perform pg_notify('dispatch_embeddings', '');
    end if;
  end;
  $enqueue$;

  create or replace function embeddings_enqueue_window(p_kind text, p_id text, p_text text, p_covers text[]) returns void
  language plpgsql as $enqueue_window$
  declare
    v_hash text := encode(sha256(convert_to(p_text, 'UTF8')), 'hex');
    v_project text := embeddings_owner_project_key(p_kind, p_id);
  begin
    perform embeddings_ensure_project_partition(v_project);
    if v_project is not null then
      update embeddings set project_key = v_project where kind = p_kind and id = p_id and project_key is null;
    end if;
    insert into embeddings (kind, id, text_snapshot, content_hash, covers, project_key)
    values (p_kind, p_id, p_text, v_hash, p_covers, v_project)
    on conflict (kind, id, project_key) do update
      set text_snapshot = excluded.text_snapshot,
          content_hash = excluded.content_hash,
          covers = excluded.covers,
          attempt_count = 0,
          confirmed_failures = 0,
          next_attempt_at = now(),
          dead = false,
          dead_model = null,
          pending_since = now()
      where embeddings.content_hash is distinct from excluded.content_hash;
    if found then
      perform pg_notify('dispatch_embeddings', '');
    end if;
  end;
  $enqueue_window$;
end;
$$;
