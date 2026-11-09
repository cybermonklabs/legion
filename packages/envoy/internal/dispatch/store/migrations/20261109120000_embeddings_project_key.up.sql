-- 20261109120000_embeddings_project_key.up.sql — the project a vector belongs to, on the row
-- that holds the vector.
--
-- Meaning search ranks each kind on its own arm, each arm `order by embedding <=> $9 limit n`
-- against embeddings (api/search.go, searchQueryMeaningBody). Every arm's project filter was a
-- predicate on a joined table - the issue's project through issues, a document's through
-- artifacts and projects, a comment's and an ask's through coalesce(issues.project_key,
-- projects.key), a message's through its issue - and the comment arm's ran above its candidate
-- limit, because the join below the ordering cost that arm the index entirely (INFRA-988, the
-- measurement searchMeaningCommentArm holds). A filter above the limit is a filter of the
-- wrong 1,000 rows: a project-scoped search reads the 1,000 nearest comment windows of every
-- project and keeps the in-project ones, so a project holding 800 windows can be answered with
-- 2 of them. This column is that filter moved onto the row the index orders, so the predicate
-- rides inside each candidate CTE and the arm's limit is spent on the project that asked.
--
-- Measured on the load-test deployment's own database with this column and this index in place
-- (1,107,674 embedded rows, 723,015 of them comment windows; LOOP 703,365 / LGA 14,850 /
-- LGB 2,400 / LGC 1,600 / LGD 800 windows), one scoped comment arm per project:
--
--   LGD, 800 windows      embeddings_kind_project, exact sort   800 of 800 rows   13.7 ms
--                         (the arm answered 2 of 800 before)                      (225 ms)
--   LGC, 1,600 windows    embeddings_kind_project, 1,000 kept   1,000 rows        22.8 ms
--   LGA, 14,850 windows   bitmap scan of embeddings_kind_project, 1,000 kept      195.1 ms
--   LOOP, 120,000 rows    embeddings_cosine, the predicate inside the iterative
--                         scan                                  1,000 rows        289.7 ms
--
-- So the planner holds two good plans and picks between them on its own statistics: an exact
-- read of one small project's rows, and the HNSW iterative scan for a large one. It never
-- chooses the HNSW index while the predicate sits above the candidate limit, which is why the
-- predicate had to come down onto the row.
--
-- The column is text and carries no foreign key to projects. A key is what the arms compare
-- ($2 is a project key), the row is derived data that a delete of a project would have to
-- cascade through 1.1 M rows to settle, and nothing reads this column but those arms.
--
-- What writes it. Every row of the table is written through one of two functions, both
-- replaced below with their signatures unchanged: embeddings_enqueue (0072, 0118, 0151) for the
-- one whole-text kind left, the issue's, and embeddings_enqueue_window (20261102233625) for the
-- four window kinds. Each now sets project_key in the same statement that writes the row, from
-- the owner the row belongs to, through embeddings_owner_project_key below. The one insert that
-- does not go through either is the coverage pass's row for an issue that has none
-- (embedqueue/coverage.go, insertMissingIssues), which selects the issue's own project_key
-- beside the text it inserts.
--
-- Resolved inside the function rather than passed in by each caller, which is what makes the
-- rolling deploy whole: the release before this one calls both functions with exactly these
-- arguments and has no project key to give (refs/blocks.go's writeWindows, and 0140's two issue
-- triggers, which this migration leaves as they are). A row it writes during the roll is
-- labelled by the function, so no window written while both releases serve is left unlabelled
-- and invisible to a scoped search until its text next changes. It costs one lookup by primary
-- key per row written - a document of fifty windows pays fifty of them inside a statement
-- measured at 114.3 ms (INFRA-988) - which is the price of that property.
--
-- A row's project never changes afterwards: nothing moves an object between projects, no
-- `update ... set project_key` and no `update ... set issue_key` exists outside the migrations
-- and the tests, and api/project_scope.go rests its whole pre-handler check on that. So the
-- value written with the row stays true for as long as the row lives.
--
-- What this costs to apply. The ALTER adds a nullable column with no default, which rewrites
-- no row and is a catalog change alone. The index is built over every row of the table, under
-- the ACCESS EXCLUSIVE lock the ALTER above already took and holds to this migration's commit,
-- so reads and writes of embeddings wait for the build: the key is two text columns of
-- 1,107,674 rows on the load-test deployment, where the deployment's own build of it is what
-- the plans above were measured on. A plain `create index`, as every migration's is - the
-- runner applies each migration inside one transaction, where `concurrently` cannot run.
-- pgmigrate.LockTimeout (5 s) bounds the wait for the lock itself, and the runner fails the
-- boot past it rather than holding every reader of embeddings behind a queued ALTER.
--
-- The pre-deploy census reports the table's size and refuses a migration touching a table over
-- pgmigrate.CensusTableLimit (1 GiB). embeddings is far over it wherever it holds a million
-- 1,536-float vectors, and there is no shape of this change that is not: a column and an index
-- are not catalog-only statements, and `create index concurrently` cannot run in the runner's
-- transaction. The refusal is the deploy's to answer, with this header's figures for what the
-- migration does while it holds the lock.
--
-- The backfill of the rows already stored is 20261109120100's, a migration of its own because
-- it is the only part of this change whose cost grows with the deployment, and that cost is an
-- hour: see its header.
alter table embeddings add column project_key text;

-- The index the scoped arms read. (kind, project_key) and in that order, because every arm
-- names its kind as a literal and the project as $2, so one kind's rows of one project are a
-- contiguous range of it. A btree, not an HNSW one: this index is what the planner uses when a
-- small project's rows are few enough to sort exactly, and the ordering by distance is
-- embeddings_cosine's (0072) job, which the LOOP row above is the planner choosing.
--
-- Partial over the five kinds meaning search reads, as embeddings_window_owner
-- (20261102233625) is partial over the four window kinds, and for a sharper reason than its
-- size: the whole-text rows it leaves out - the retired 'comment', 'message', 'ask' and
-- 'document' kinds the block index deletes as each owner gains a current window - are read by
-- one statement, the retirement's own delete, which must reach each row by its primary key
-- and not by its kind (docs/block_index.go, retireWholeText). An index on (kind, …) that held
-- those rows offers the planner a read of every row of such a kind, and it took it:
-- docs.TestRetirementReachesEachWholeTextRowThroughItsPrimaryKey fails on exactly that plan.
-- Outside that list the index therefore does not exist, and inside it every arm proves the
-- predicate from the kind literal it already names.
create index embeddings_kind_project on embeddings (kind, project_key)
  where kind in ('issue', 'document_block', 'comment_block', 'message_block', 'ask_part');

-- embeddings_owner_project_key is the project of the item an embeddings row stands for, read
-- from that item, and null where the item is gone or its kind owns no project. It is the one
-- place that rule is written: the two write functions below call it, and 20261109120100's fill
-- labels the stored rows with it.
--
-- The rules are the five meaning arms' own project predicates, which this column replaces, each
-- taken from api/search.go's searchQueryMeaningBody as it stood before:
--
--   issue           i.project_key                              of the issue whose key is the id
--   document_block  p.key, where p.key = a.project_key         of the artifact whose window it is
--   comment_block   coalesce(i.project_key, p.key)             of the comment's issue, else of
--                                                              the document it is anchored to
--   message_block   i.project_key                              of the message's issue
--   ask_part        coalesce(i.project_key, p.key)             of the ask's issue, else of the
--                                                              document holding its block
--
-- The arms reached projects p through `join projects p on p.key = a.project_key`, so p.key is
-- a.project_key wherever the artifact exists and null where it does not; this reads the column
-- and leaves the join out, which answers the same value for one less probe per row.
--
-- A window's id is "<owner key>#<the first block it covers>", and the owner key is the artifact
-- id for a document's window and "<kind>/<uuid>" for a comment's, a message's or an ask's
-- (refs.ItemKind.WindowOwnerKey). The cast of either is guarded by the same regex
-- store.CanonicalUUID spells out, so an id in any other shape resolves to no item and a null
-- project rather than answering SQLSTATE 22P02 for the whole write.
create function embeddings_owner_project_key(p_kind text, p_id text) returns text
language plpgsql stable as $$
declare
  v_owner text := split_part(p_id, '#', 1);
  v_uuid uuid;
begin
  if p_kind = 'issue' then
    return (select i.project_key from issues i where i.key = p_id);
  end if;
  if p_kind = 'document_block' then
    v_uuid := case when v_owner ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                   then v_owner::uuid end;
    return (select a.project_key from artifacts a where a.id = v_uuid);
  end if;
  v_uuid := case when split_part(v_owner, '/', 2) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                 then split_part(v_owner, '/', 2)::uuid end;
  if p_kind = 'comment_block' then
    return (select coalesce(i.project_key, a.project_key)
              from comments c
              left join issues i on i.key = c.issue_key
              left join artifacts a on a.id = c.artifact_id
             where c.id = v_uuid);
  elsif p_kind = 'message_block' then
    return (select i.project_key from messages m join issues i on i.key = m.issue_key where m.id = v_uuid);
  elsif p_kind = 'ask_part' then
    return (select coalesce(i.project_key, a.project_key)
              from asks k
              left join issues i on i.key = k.issue_key
              left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
             where k.id = v_uuid);
  end if;
  return null;
end;
$$;

-- embeddings_enqueue is 0151's, with project_key written beside the text and kept in step with
-- it: the whole-text path, which the two issue triggers of 0140 call. The upsert sets it on the
-- row it inserts and again on a text change, under the same guard as every other column - a
-- write that leaves the text as it was writes nothing at all - so a row already labelled keeps
-- its label, its vector, its retry state and its wait exactly as a no-op upsert left them
-- before this migration.
create or replace function embeddings_enqueue(p_kind text, p_id text, p_text text) returns void
language plpgsql as $$
declare
  v_hash text := encode(sha256(convert_to(p_text, 'UTF8')), 'hex');
begin
  insert into embeddings (kind, id, text_snapshot, content_hash, project_key)
  values (p_kind, p_id, p_text, v_hash, embeddings_owner_project_key(p_kind, p_id))
  on conflict (kind, id) do update
    set text_snapshot = excluded.text_snapshot,
        content_hash = excluded.content_hash,
        project_key = excluded.project_key,
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
$$;

-- embeddings_enqueue_window is 20261102233625's, with project_key written the same way. Its
-- signature is unchanged, so refs.writeWindows' one statement is unchanged too and the release
-- before this one keeps writing labelled rows through it (this file's header says why that
-- matters).
create or replace function embeddings_enqueue_window(p_kind text, p_id text, p_text text, p_covers text[]) returns void
language plpgsql as $$
declare
  v_hash text := encode(sha256(convert_to(p_text, 'UTF8')), 'hex');
begin
  insert into embeddings (kind, id, text_snapshot, content_hash, covers, project_key)
  values (p_kind, p_id, p_text, v_hash, p_covers, embeddings_owner_project_key(p_kind, p_id))
  on conflict (kind, id) do update
    set text_snapshot = excluded.text_snapshot,
        content_hash = excluded.content_hash,
        covers = excluded.covers,
        project_key = excluded.project_key,
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
$$;
