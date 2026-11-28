-- 20261124011834_search_coverage_counts.up.sql — the counts GET /api/v1/search/coverage reads,
-- kept by triggers on the tables they count.
--
-- Why it exists. The coverage route counted every item of the five covered kinds and every row
-- of the not-indexed tables in one statement (main's statement: coverageQuery at df18d9be, kept as
-- mainRouteCoverageStatement in embedqueue/testdata/main_route_coverage_statement.sql), which reads
-- every window row of embeddings and so grows with the corpus: from 2026-11-21T08:19Z to
-- 2026-11-22T00:30Z, 10 of 65 probe calls reached its 10 s bound. Postgres keeps no exact count of
-- a table's rows, so the exact answer that does not grow is a count that each write keeps in its
-- own transaction. This migration makes the tables that hold those counts and the triggers that
-- keep them. embedqueue's counts loop fills them, folds them every 10 s and recounts them, each
-- recount starting at most 11 minutes less its bound (coverageRecountTimeout) after the one
-- before started, so a write the triggers missed is counted within 11 minutes; the route reads
-- them and never runs main's statement.
--
-- The tables:
--
--   - search_coverage_counts, the folded base: one row per class, with how many items it holds.
--   - search_coverage_deltas, one row per class a write statement moved, items signed. The fold
--     deletes the rows it sees and adds their sums to the base in one transaction, and the route
--     sums the base and the rows not yet folded.
--   - search_coverage_issues, one row per issue: the issue arm of main's statement kept per issue.
--     has_text says the issue is an item; inputs is what the row was computed from
--     (search_coverage_issue_inputs), so the recount finds an issue that a write changed without
--     its triggers.
--   - search_coverage_filled, the one row the fill inserts when it ends, with the moment
--     (done_at). The migration inserts no row, so a table that holds none says the counts are not
--     filled yet.
--
-- A class is (kind, model, matched, dead), each as main's statement reads it (main at df18d9be:
-- mainRouteCoverageStatement, embedqueue/testdata/main_route_coverage_statement.sql). A row of
-- embeddings is an item of its kind when the kind is a window kind and its text has
-- something to embed (embedqueue's windowItems, word for word: a change to the window kinds or to
-- the blank test owes a migration that replaces search_coverage_count_embeddings); matched is
-- embedding is not null and embedded_hash = content_hash; dead is the row's. An issue is an item
-- when its text has something to embed, as that statement's issue arm tests it; matched is a
-- vector at the hash that arm computes; dead is coalesce(e.dead, false); waited_since is
-- greatest(e.pending_since, i.created_at). model is the row's model where matched and ''
-- otherwise. A not-indexed kind (component, document_version, result, handoff) is one class:
-- model '', matched and dead false; each counts every row of its table (notIndexedKinds), and
-- route_roster's session count is read from its one row and kept by nothing here. The route's
-- current count for an identity is the sum of the classes matched with that model.
--
-- Why delta rows and not one counter row per class. An issue's text reaches embeddings inside the
-- API write's own transaction (0140's triggers), and the embed commit and the window writer write
-- embeddings tens of thousands of times a day. A counter row updated in place would hold its row
-- lock to the commit of each such transaction, order those transactions behind each other, and two
-- transactions updating two classes in opposite orders would deadlock. A delta row is an insert:
-- no writer waits on another.
--
-- The delta table's autovacuum settings. Each fold deletes every delta row it sees and so leaves
-- each of them dead, while the table's live rows stay near zero. Under the default scale factor
-- autovacuum waits for a share of a table that is nearly empty; with
-- autovacuum_vacuum_scale_factor = 0 it vacuums after every 10,000 dead rows, whatever the
-- table's size. 10,000 is a tenth of the route's bound on the delta rows it reads (100,000), so
-- that read does not walk a pile of dead tuples.
--
-- The triggers are ordinary triggers. The census admits no ENABLE ALWAYS TRIGGER (pgmigrate's
-- catalogOnlyAlterTable), so a write under session_replication_role = replica (a restore, a
-- logical-replication apply, embedqueue's withTriggersOff) or with `alter table … disable trigger
-- user` fires none of them; the recount follows such a write within 11 minutes (R7). Postgres
-- takes transition tables only on a trigger of one event and with no column list, so a statement
-- trigger that reads its rows is one trigger per event, and those of one table share a function:
--
--   - embeddings, after insert, after update and after delete, for each statement: one delta row
--     per window class the statement moved, its old rows minus and its new rows plus, so an update
--     that leaves each row in its class writes none; then the refresh of each issue a row of kind
--     issue names, on update only where the row's embedding is null, embedded_hash, model, dead
--     or pending_since changed between its old row and its new one, so a claim, which moves
--     next_attempt_at alone (embedqueue's scanPending and claimRenewal), refreshes nothing.
--   - components, artifact_versions, check_results and handoffs, after insert and after delete,
--     for each statement: one delta row of the rows added or removed, under the table's kind.
--   - issues (after insert, update of title or created_at, and delete), artifacts (after insert,
--     update of issue_key or is_primary, and delete) and artifact_versions (after insert, update
--     of markdown, and delete), for each row: the refresh of the issue whose class the row is read
--     into, the issue itself, the issue a primary document is the spec of, and the issue whose
--     primary document holds the version.
--   - search_coverage_issues, for each row: the old class minus and the new class plus as delta
--     rows of kind issue, none when the class stays.
--   - each of those seven tables, after truncate: the base rows of the table's kinds go to 0 and
--     their delta rows go; on embeddings, artifacts and artifact_versions every issue is refreshed,
--     its row or its spec being gone; on issues every state row is deleted, whose own trigger
--     writes the matching delta rows.
--
-- The refresh, search_coverage_issue_refresh, computes an issue's row as the issue arm of main's
-- statement computes it, its shortcut to the hash through embedding_issue_sources included, and
-- writes it to the issue's state row, or deletes the row of an issue that is gone. It is exact
-- under READ COMMITTED whatever the interleaving of the writes: it first takes the lock of the
-- issue's state row, inserting the key as a row no class counts (has_text false) where the issue
-- has none, and computes the row only in a later statement, whose snapshot is taken after every
-- transaction that held that lock before it has committed. Every write of an input refreshes
-- after it in its own transaction, so of two transactions that change two inputs of one issue,
-- the one that takes the lock second computes after the other has committed and sees both
-- changes. A statement that refreshes several issues refreshes them in key order. inputs is
-- search_coverage_issue_inputs over issueSource's value, the issue's created_at and its embeddings
-- row's id and xmin, the one rendering the recount compares too; a write to any of them, with or
-- without triggers, changes a value or a row's xmin. A claim changes its row's xmin and refreshes
-- nothing, the issue's class being as it was, so the recount finds that issue's inputs changed
-- and writes them again. content_hash is not among the columns an update of embeddings is
-- refreshed for: each write that changes it sets pending_since = now() beside it
-- (embeddings_enqueue, embedqueue's repairStaleIssues), and where now() repeats, inside one
-- transaction, the write of the title or the spec that made the new text refreshes the issue
-- itself after its enqueue, since Postgres fires one table's row triggers in the order of their
-- names. A later migration that changes issue_embedding_text or issue_embedding_spec must refresh
-- every issue's row in the same release, as it must empty embedding_issue_sources: inputs names
-- the inputs, not the function that turns them into text.
--
-- Every name is written with its schema and every function the bodies call with pg_catalog, as
-- 20261115200014 writes its own: these functions run under the caller's search_path.
--
-- What it locks. Each create trigger takes SHARE ROW EXCLUSIVE on its table and holds it to the
-- commit: a read of the table waits for nothing, and a write of it waits for the migration to
-- commit. The migration reads no row, so it holds those locks for as long as its catalog writes
-- take. The functions come first, since creating one writes pg_proc and locks no table, and the
-- triggers after them, table by table in one fixed order: issues, artifacts, artifact_versions,
-- embeddings, components, check_results, handoffs, then search_coverage_issues, the order the
-- server's own writes take them in (an issue's write, then its document's, then 0140's enqueue
-- into embeddings), as 20261101182522_search_term_queue_triggers takes its five in one order. The
-- migration still holds each table's lock while it waits for the next, so a writer that takes two
-- of these tables in another order closes a cycle with it. The lock_timeout below, 500 ms, is
-- shorter than deadlock_timeout (1 s by default), so against any such writer the migration gives
-- up first, before either deadlock check runs: it fails with 55P03 having applied nothing, the
-- writer finishes, and pgmigrate applies it again, up to pgmigrate.LockTimeoutAttempts times in
-- the same boot, then at the next boot. Production runs one replica under Recreate, so no writer
-- of this server's own runs during the migration there; the rule holds for two builds at once,
-- for several processes and for a writer outside the server. It carries 20261101182522's cost
-- too: Postgres cancels an autovacuum that holds a lock only after the waiter has waited
-- deadlock_timeout, so an autovacuum of one of the seven tables outlasts the migration's wait, and
-- the census refuses the migration while one holds a lock on them (internal/pgmigrate/census.go,
-- cancelsAutovacuum). The four new tables no other session can reach before the commit. Once
-- applied, a write that refreshes an issue holds that issue's state row until it commits, so two
-- writers of one issue's inputs take that row in turn; the writes of Dispatch that change an
-- issue's text take its embeddings row first (embeddings_enqueue), as the embed commit does, so
-- each takes the two rows in one order.
--
-- Why the census admits it. Each statement is a catalog-only kind (pgmigrate's
-- catalogOnlyStatements): a set of lock_timeout, which bounds the migration's own wait and keeps
-- the exemption the rest earn; create table, with no INHERITS or PARTITION OF, the storage
-- parameters of the delta table included, since a new table holds no row; create index on a table
-- this migration creates; create function in plpgsql or sql; and create trigger, which runs only
-- on later writes, never on the rows already there. So it applies whatever the size of embeddings
-- and the six other tables, and the census answers 0.
--
-- During a roll and after one. The release before this one neither reads nor writes the new
-- tables, and the triggers are the database's, so its writes keep the counts as well. Rolled back
-- past this release, the triggers stay and keep writing delta rows that no process folds, a few
-- for each write statement, until a release with the counts loop runs again.
set local lock_timeout = '500ms';

create table public.search_coverage_counts (
  kind text not null,
  model text not null,
  matched boolean not null,
  dead boolean not null,
  items bigint not null,
  primary key (kind, model, matched, dead)
);

create table public.search_coverage_deltas (
  id bigint generated always as identity primary key,
  kind text not null,
  model text not null,
  matched boolean not null,
  dead boolean not null,
  items bigint not null
) with (autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 10000);

create table public.search_coverage_issues (
  key text primary key,
  has_text boolean not null,
  matched boolean not null,
  model text not null,
  dead boolean not null,
  waited_since timestamptz not null,
  inputs text not null
);

-- The oldest lacking issue, as the window indexes give the oldest lacking window: the first entry
-- of `not dead` among the items with no vector of their text, and among the matched items of each
-- model other than the identity's, never walking the current ones.
create index search_coverage_issues_unmatched on public.search_coverage_issues (dead, waited_since)
  where has_text and not matched;
create index search_coverage_issues_matched on public.search_coverage_issues (dead, model, waited_since)
  where has_text and matched;

create table public.search_coverage_filled (
  id boolean primary key default true check (id),
  done_at timestamptz not null
);

-- An issue's inputs as search_coverage_issues records them: created_at as seconds since the epoch,
-- which no session's TimeZone or DateStyle changes, the embeddings row's id and xmin ('-' for each
-- with no row), and issueSource's value (embedqueue/coverage.go), which ends with the title, so no
-- two sets of inputs render alike.
create function public.search_coverage_issue_inputs(p_source text, p_created_at timestamptz, p_embedding_id text, p_embedding_xmin xid)
returns text
language sql stable parallel safe as $$
  select extract(epoch from p_created_at)::text || ':' || coalesce(p_embedding_id, '-') || ':'
    || coalesce(p_embedding_xmin::text, '-') || ':' || p_source
$$;

-- issueSourceFrom and issueSource are embedqueue's, word for word but for the schema each name is
-- written with, and TestTheRefreshAndTheRecountRenderAnIssuesInputsAlike
-- (embedqueue/coverage_counts_test.go) holds the two renderings equal; the text test and the hash
-- are those of the issue arm of main's statement (mainRouteCoverageStatement,
-- embedqueue/testdata/main_route_coverage_statement.sql, main at df18d9be). The loop takes the
-- state row's lock: a row another transaction inserted after the select's snapshot is not seen by
-- it, so the insert meets that row and does nothing and the loop selects it again under a new
-- snapshot; a row another transaction deleted while the select waited for it is gone, so the
-- insert makes it again.
create function public.search_coverage_issue_refresh(p_key text) returns void
language plpgsql as $$
begin
  loop
    perform 1 from public.search_coverage_issues where key = p_key for update;
    exit when found;
    insert into public.search_coverage_issues (key, has_text, matched, model, dead, waited_since, inputs)
    values (p_key, false, false, '', false, pg_catalog.now(), '')
    on conflict (key) do nothing;
  end loop;
  perform 1 from public.issues where key = p_key;
  if not found then
    delete from public.search_coverage_issues where key = p_key;
    return;
  end if;
  update public.search_coverage_issues s
  set has_text = issue.has_text, matched = issue.matched, model = issue.model,
      dead = issue.dead, waited_since = issue.waited_since, inputs = issue.inputs
  from (
    select c.key, c.has_text, c.matched,
           case when c.matched then coalesce(c.model, '') else '' end as model,
           c.dead, c.waited_since, c.inputs
    from (
      select i.key,
             (pg_catalog.ascii(i.title) between 33 and 126
              or public.issue_embedding_text(i.title, public.issue_embedding_spec(i.key)) ~ '[^[:space:]]') as has_text,
             coalesce(e.embedding is not null and e.embedded_hash = case
               when r.source = src.issue_source then e.content_hash
               else pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(
                 public.issue_embedding_text(i.title, public.issue_embedding_spec(i.key)), 'UTF8')), 'hex')
             end, false) as matched,
             e.model,
             coalesce(e.dead, false) as dead,
             greatest(e.pending_since, i.created_at) as waited_since,
             public.search_coverage_issue_inputs(src.issue_source, i.created_at, e.id, e.xmin) as inputs
      from public.issues i
      left join lateral (
        select v.id, v.xmin
        from public.artifact_versions v
        where v.artifact_id = (select a.id from public.artifacts a where a.issue_key = i.key and a.is_primary)
        order by v.number desc
        limit 1
      ) spec on true
      left join public.embeddings e on e.kind = 'issue' and e.id = i.key
      left join public.embedding_issue_sources r on r.key = i.key
      cross join lateral (
        select coalesce(spec.id::text, '-') || ':' || coalesce(spec.xmin::text, '-') || ':'
          || coalesce(e.content_hash, '-') || ':' || i.title as issue_source
      ) src
      where i.key = p_key
    ) c
  ) issue
  where s.key = issue.key
    and (s.has_text, s.matched, s.model, s.dead, s.waited_since, s.inputs)
      is distinct from (issue.has_text, issue.matched, issue.model, issue.dead, issue.waited_since, issue.inputs);
end $$;

-- An issue's class moved: the old one minus and the new one plus, of an item only.
create function public.search_coverage_issue_classes() returns trigger
language plpgsql as $$
begin
  if tg_op = 'UPDATE' then
    if (old.has_text, old.model, old.matched, old.dead) is not distinct from (new.has_text, new.model, new.matched, new.dead) then
      return null;
    end if;
  end if;
  if tg_op <> 'INSERT' then
    if old.has_text then
      insert into public.search_coverage_deltas (kind, model, matched, dead, items)
      values ('issue', old.model, old.matched, old.dead, -1);
    end if;
  end if;
  if tg_op <> 'DELETE' then
    if new.has_text then
      insert into public.search_coverage_deltas (kind, model, matched, dead, items)
      values ('issue', new.model, new.matched, new.dead, 1);
    end if;
  end if;
  return null;
end $$;

-- A statement's rows of embeddings: the window classes it moved, and the issues whose rows it
-- inserted or deleted, or, for an update, whose rows it changed in a column an issue's class or
-- wait is read from (embedding is null, embedded_hash, model, dead, pending_since), each paired
-- with its old row by id. Each branch reads the transition tables its trigger names; plpgsql plans
-- a statement only when it first runs it, so a branch another trigger takes is never planned
-- against tables it lacks.
create function public.search_coverage_count_embeddings() returns trigger
language plpgsql as $$
begin
  if tg_op = 'INSERT' then
    insert into public.search_coverage_deltas (kind, model, matched, dead, items)
    select w.kind, case when w.matched then coalesce(w.model, '') else '' end, w.matched, w.dead, pg_catalog.count(*)
    from (select r.kind, r.model, r.dead, coalesce(r.embedding is not null and r.embedded_hash = r.content_hash, false) as matched
          from new_rows r
          where r.kind ~ '^(document_block|comment_block|message_block|ask_part)$' and r.text_snapshot ~ '[^[:space:]]') w
    group by 1, 2, 3, 4;
    perform public.search_coverage_issue_refresh(k.id)
    from (select distinct r.id from new_rows r where r.kind = 'issue') k
    order by k.id;
  elsif tg_op = 'UPDATE' then
    insert into public.search_coverage_deltas (kind, model, matched, dead, items)
    select w.kind, case when w.matched then coalesce(w.model, '') else '' end, w.matched, w.dead, pg_catalog.sum(w.items)
    from (select r.kind, r.model, r.dead, coalesce(r.embedding is not null and r.embedded_hash = r.content_hash, false) as matched, -1 as items
          from old_rows r
          where r.kind ~ '^(document_block|comment_block|message_block|ask_part)$' and r.text_snapshot ~ '[^[:space:]]'
          union all
          select r.kind, r.model, r.dead, coalesce(r.embedding is not null and r.embedded_hash = r.content_hash, false), 1
          from new_rows r
          where r.kind ~ '^(document_block|comment_block|message_block|ask_part)$' and r.text_snapshot ~ '[^[:space:]]') w
    group by 1, 2, 3, 4
    having pg_catalog.sum(w.items) <> 0;
    perform public.search_coverage_issue_refresh(k.id)
    from (select coalesce(o.id, n.id) as id
          from (select r.id, r.embedding is null as unembedded, r.embedded_hash, r.model, r.dead, r.pending_since
                from old_rows r where r.kind = 'issue') o
          full join (select r.id, r.embedding is null as unembedded, r.embedded_hash, r.model, r.dead, r.pending_since
                     from new_rows r where r.kind = 'issue') n on n.id = o.id
          where (o.id, o.unembedded, o.embedded_hash, o.model, o.dead, o.pending_since)
            is distinct from (n.id, n.unembedded, n.embedded_hash, n.model, n.dead, n.pending_since)) k
    order by k.id;
  else
    insert into public.search_coverage_deltas (kind, model, matched, dead, items)
    select w.kind, case when w.matched then coalesce(w.model, '') else '' end, w.matched, w.dead, -pg_catalog.count(*)
    from (select r.kind, r.model, r.dead, coalesce(r.embedding is not null and r.embedded_hash = r.content_hash, false) as matched
          from old_rows r
          where r.kind ~ '^(document_block|comment_block|message_block|ask_part)$' and r.text_snapshot ~ '[^[:space:]]') w
    group by 1, 2, 3, 4;
    perform public.search_coverage_issue_refresh(k.id)
    from (select distinct r.id from old_rows r where r.kind = 'issue') k
    order by k.id;
  end if;
  return null;
end $$;

-- A statement's rows of a not-indexed table, under the kind its trigger names (tg_argv[0]).
create function public.search_coverage_count_rows() returns trigger
language plpgsql as $$
begin
  if tg_op = 'INSERT' then
    insert into public.search_coverage_deltas (kind, model, matched, dead, items)
    select tg_argv[0], '', false, false, pg_catalog.count(*) from new_rows having pg_catalog.count(*) > 0;
  else
    insert into public.search_coverage_deltas (kind, model, matched, dead, items)
    select tg_argv[0], '', false, false, -pg_catalog.count(*) from old_rows having pg_catalog.count(*) > 0;
  end if;
  return null;
end $$;

-- An issue's own row: its key, and its old key where an update moved it.
create function public.search_coverage_issues_refresh() returns trigger
language plpgsql as $$
begin
  if tg_op = 'DELETE' then
    perform public.search_coverage_issue_refresh(old.key);
    return null;
  end if;
  perform public.search_coverage_issue_refresh(new.key);
  if tg_op = 'UPDATE' then
    if old.key <> new.key then
      perform public.search_coverage_issue_refresh(old.key);
    end if;
  end if;
  return null;
end $$;

-- A primary document's issue, before the write and after it.
create function public.search_coverage_artifacts_refresh() returns trigger
language plpgsql as $$
declare
  v_old text;
  v_new text;
begin
  if tg_op <> 'INSERT' then
    if old.is_primary then
      v_old := old.issue_key;
    end if;
  end if;
  if tg_op <> 'DELETE' then
    if new.is_primary then
      v_new := new.issue_key;
    end if;
  end if;
  if v_old is not null and v_old is distinct from v_new then
    perform public.search_coverage_issue_refresh(v_old);
  end if;
  if v_new is not null then
    perform public.search_coverage_issue_refresh(v_new);
  end if;
  return null;
end $$;

-- The issue whose primary document holds the version, before the write and after it.
create function public.search_coverage_artifact_versions_refresh() returns trigger
language plpgsql as $$
declare
  v_old text;
  v_new text;
begin
  if tg_op <> 'INSERT' then
    select a.issue_key into v_old from public.artifacts a where a.id = old.artifact_id and a.is_primary;
  end if;
  if tg_op <> 'DELETE' then
    select a.issue_key into v_new from public.artifacts a where a.id = new.artifact_id and a.is_primary;
  end if;
  if v_old is not null and v_old is distinct from v_new then
    perform public.search_coverage_issue_refresh(v_old);
  end if;
  if v_new is not null then
    perform public.search_coverage_issue_refresh(v_new);
  end if;
  return null;
end $$;

-- A truncate of a counted table: the base rows of the kinds its trigger names (tg_argv) go to 0
-- and their delta rows go, and the issues' rows follow what the truncate took from them. A
-- truncate takes ACCESS EXCLUSIVE on its table, so no write of that table is still open; a fold
-- that holds a delta row or a base row this trigger writes makes it wait, and it then reads that
-- row as the fold left it.
create function public.search_coverage_truncated() returns trigger
language plpgsql as $$
begin
  if tg_nargs > 0 then
    update public.search_coverage_counts set items = 0 where kind = any (tg_argv) and items <> 0;
    delete from public.search_coverage_deltas where kind = any (tg_argv);
  end if;
  if tg_table_name = 'issues' then
    delete from public.search_coverage_issues;
  elsif tg_table_name in ('embeddings', 'artifacts', 'artifact_versions') then
    perform public.search_coverage_issue_refresh(i.key) from public.issues i order by i.key;
  end if;
  return null;
end $$;

-- The triggers, table by table in the order "What it locks" names.
create trigger search_coverage_issues_rows after insert or update of title, created_at or delete on public.issues
  for each row execute function public.search_coverage_issues_refresh();
create trigger search_coverage_issues_truncate after truncate on public.issues
  for each statement execute function public.search_coverage_truncated();

create trigger search_coverage_artifacts_rows after insert or update of issue_key, is_primary or delete on public.artifacts
  for each row execute function public.search_coverage_artifacts_refresh();
create trigger search_coverage_artifacts_truncate after truncate on public.artifacts
  for each statement execute function public.search_coverage_truncated();

create trigger search_coverage_artifact_versions_insert after insert on public.artifact_versions
  referencing new table as new_rows
  for each statement execute function public.search_coverage_count_rows('document_version');
create trigger search_coverage_artifact_versions_delete after delete on public.artifact_versions
  referencing old table as old_rows
  for each statement execute function public.search_coverage_count_rows('document_version');
create trigger search_coverage_artifact_versions_rows after insert or update of markdown or delete on public.artifact_versions
  for each row execute function public.search_coverage_artifact_versions_refresh();
create trigger search_coverage_artifact_versions_truncate after truncate on public.artifact_versions
  for each statement execute function public.search_coverage_truncated('document_version');

create trigger search_coverage_embeddings_insert after insert on public.embeddings
  referencing new table as new_rows
  for each statement execute function public.search_coverage_count_embeddings();
create trigger search_coverage_embeddings_update after update on public.embeddings
  referencing old table as old_rows new table as new_rows
  for each statement execute function public.search_coverage_count_embeddings();
create trigger search_coverage_embeddings_delete after delete on public.embeddings
  referencing old table as old_rows
  for each statement execute function public.search_coverage_count_embeddings();
create trigger search_coverage_embeddings_truncate after truncate on public.embeddings
  for each statement execute function public.search_coverage_truncated('document_block', 'comment_block', 'message_block', 'ask_part');

create trigger search_coverage_components_insert after insert on public.components
  referencing new table as new_rows
  for each statement execute function public.search_coverage_count_rows('component');
create trigger search_coverage_components_delete after delete on public.components
  referencing old table as old_rows
  for each statement execute function public.search_coverage_count_rows('component');
create trigger search_coverage_components_truncate after truncate on public.components
  for each statement execute function public.search_coverage_truncated('component');

create trigger search_coverage_check_results_insert after insert on public.check_results
  referencing new table as new_rows
  for each statement execute function public.search_coverage_count_rows('result');
create trigger search_coverage_check_results_delete after delete on public.check_results
  referencing old table as old_rows
  for each statement execute function public.search_coverage_count_rows('result');
create trigger search_coverage_check_results_truncate after truncate on public.check_results
  for each statement execute function public.search_coverage_truncated('result');

create trigger search_coverage_handoffs_insert after insert on public.handoffs
  referencing new table as new_rows
  for each statement execute function public.search_coverage_count_rows('handoff');
create trigger search_coverage_handoffs_delete after delete on public.handoffs
  referencing old table as old_rows
  for each statement execute function public.search_coverage_count_rows('handoff');
create trigger search_coverage_handoffs_truncate after truncate on public.handoffs
  for each statement execute function public.search_coverage_truncated('handoff');

create trigger search_coverage_issue_classes after insert or update or delete on public.search_coverage_issues
  for each row execute function public.search_coverage_issue_classes();
