-- 20261117025900_refs_forms_owed.up.sql
-- The queue of text owed an index with the reference forms this release adds to the grammar: an
-- issue's tab addresses (`dispatch://CORE-45/plan`, `/verification`, `/activity`, `/children`,
-- `/artifacts`, `/collaborators`, `/conversation`, `/log`, and the same as dashboard URLs) and the
-- dashboard URLs of an issue's document (`https://<origin>/issues/CORE-50/artifact/notes-md`, with
-- `?v=` and `#b-`). A build that predates them reads none of them, so when it rewrites a text it
-- deletes the refs rows those forms wrote. The triggers of the four migrations after this one
-- enqueue every write any build makes, and the seed after them enqueues the text already stored,
-- so a text an older build indexed gets those edges back with no edit and no command run. A row
-- stays owed until a build that reads these forms has indexed that text itself.
--
-- A queue of its own rather than a row in 20261103205037's refs_grammar_owed, for three reasons
-- read in that queue's code:
--   - a server of the previous release drains refs_grammar_owed and settles a row by (kind, id)
--     alone, when it writes that source or drains it with the grammar it knows, so during a
--     rolling update it would consume this step's rows and no build would index those forms;
--   - this build drains refs_grammar_owed inside the block-index pass, under the block-index
--     lease, and a period one process has served is skipped by the others, so every period an
--     older process won would drain nothing of this step;
--   - that drain settles a document's row by owing it a block index, which any build's block
--     index may then take and write with the grammar it knows.
-- It knows nothing of this table, so what is enqueued here waits for a build that reads it, and
-- this build drains it in a duty of its own (docs.Service.RunRefsFormsReconciliation).
--
-- No `scope` kind: these forms change no `#id`, so a question block's settlement moves no edge of
-- theirs, and every row names one source.
--
-- Its own migration: it creates a table, an index and two functions no trigger calls yet, so it
-- takes no existing table's lock, and the four trigger migrations after it each hold one table's
-- own (0121-0125's and 20261103205037-20261103205041's precedent).
create table refs_forms_owed (
  kind text not null check (kind in ('comment', 'ask', 'message', 'artifact')),
  id uuid not null,
  enqueued_at timestamptz not null default now(),
  primary key (kind, id)
);
-- The index the drain reads through, on the whole order it reads in (refs.ListOwed, `order by
-- enqueued_at, kind, id limit $1`): the seed below gives every row of the corpus one enqueued_at,
-- so an index on that column alone would leave each pass sorting the entire backlog before its
-- limit could cut it (refs_grammar_owed_enqueued's reason).
create index refs_forms_owed_enqueued on refs_forms_owed (enqueued_at, kind, id);

-- The trigger function of the comments, asks and messages triggers: the row's kind is the
-- trigger's argument, and a write that lands on a row already owed moves its enqueued_at, so a
-- drain that read the earlier time leaves the row owed (refs.SettleOwed).
create or replace function refs_forms_owe() returns trigger language plpgsql as $$
begin
  insert into refs_forms_owed (kind, id, enqueued_at) values (tg_argv[0], new.id, now())
  on conflict (kind, id) do update set enqueued_at = excluded.enqueued_at;
  return null;
end;
$$;

-- A document version owes its document: the version's own text is the text read again.
create or replace function refs_forms_owe_version() returns trigger language plpgsql as $$
begin
  insert into refs_forms_owed (kind, id, enqueued_at) values ('artifact', new.artifact_id, now())
  on conflict (kind, id) do update set enqueued_at = excluded.enqueued_at;
  return null;
end;
$$;
