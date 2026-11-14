-- 20261110064913_embeddings_project_key_label.up.sql — a row inserted with no project takes its
-- owner's, in the database, so no writer of any release can leave a null behind the fill.
--
-- 20261109120000 has both write functions set project_key in the statement that writes the row,
-- and 20261109120100's fill labels the rows stored before them. A project-scoped meaning search
-- reads the column alone from the moment that fill records itself done (api/search.go,
-- meaningProjectScope). So a null written after that moment is different from a null written
-- before it: the fill is over and runs no more, no later write labels a row whose text does not
-- change, and the row is one no project-scoped meaning search can return for as long as it
-- lives.
--
-- Every writer of embeddings, which is what this migration had to be decided from (INFRA-988,
-- `insert into embeddings` and `update embeddings` across the tree and the migrations):
--
--   embeddings_enqueue (0072, 0118, 0151)              the one whole-text kind, the issue's;
--                                                      20261109120000 has it set project_key
--   embeddings_enqueue_window (20261102233625)         the four window kinds; the same
--   embedqueue's insertMissingIssues (coverage.go)     this release's names the column
--   embedqueue's repairStaleIssues, dropForeignVectors,
--     readmitForeignDead, settleDeadNothingToEmbed and
--     the queue's claim, settle, store and retry
--     statements (queue.go)                            update text, vectors and retry state
--                                                      alone, insert no row, name no project_key
--   cmd/dispatch-seed (cmd/dispatch-seed/embeddings.go) writes vectors into existing rows
--   0073's and 0140's triggers, 20261102233625's callers (refs.writeWindows)
--                                                      reach the table only through the two
--                                                      functions above
--
-- and the release before this one, which serves beside it for the whole of a roll's drain and
-- for as long as a rollback to it lasts. That release writes through the same two functions,
-- which this database has already replaced, so its windows and issue rows are labelled - but its
-- coverage insert is its own Go, and at 0bf41f394392caa9451ab8d6973e14d4d98c8e1d
-- (embedqueue/coverage.go, insertMissingIssues) it names (kind, id, text_snapshot, content_hash)
-- and no project_key. Every row it adds for an issue that has none is therefore inserted null,
-- and it can run at any moment of that release's life, the moments after the fill has recorded
-- itself done included. That is the one hole, and this closes it.
--
-- In the database and not in that Go, because the Go that writes the null belongs to a release
-- that has already shipped: nothing this repository does now changes what a process of it writes
-- next. A rule the table itself holds covers every writer, including one a later change adds
-- without reading 20261109120000's header.
--
-- BEFORE INSERT and not also BEFORE UPDATE. No statement anywhere writes project_key on an
-- update except the fill's own, which writes a value, and the two enqueue functions' `on
-- conflict do update`, which take it from the row the insert proposed - this trigger's output.
-- An update trigger would fire for every vector written, which is every row the queue embeds,
-- to find a column no update of that statement names.
--
-- What it costs the write path: one trigger call per row inserted, which tests one column for
-- null, and the owner lookup only where that column is null. The two enqueue functions resolve
-- the owner themselves and insert a value, so their rows pay the null test alone. A row whose
-- owner resolves to nothing (a deleted issue, an id in no shape a uuid cast takes) pays a second
-- lookup that answers null again, and stays null: no rule can give such a row a project, and no
-- meaning arm would have returned it either (20261109120000's own header on
-- embeddings_owner_project_key).
--
-- This migration creates one function and one trigger. CREATE TRIGGER takes SHARE ROW EXCLUSIVE
-- on embeddings for the catalog write alone - a write of the table waits behind it, a read does
-- not - and rewrites no row, so its census answers 0.
create function embeddings_label_project_key() returns trigger language plpgsql as $$
begin
  if new.project_key is null then
    new.project_key := embeddings_owner_project_key(new.kind, new.id);
  end if;
  return new;
end;
$$;

create trigger embeddings_project_key_label before insert on embeddings
  for each row execute function embeddings_label_project_key();
