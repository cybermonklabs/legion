-- 20261126040000_refs_decisions_forms_owed.up.sql
-- The queues of text owed an index with the reading of an issue's Decisions page this release
-- adds: `dispatch://CORE-45/decisions` and the page's own link, `/issues/CORE-45/decisions` on
-- this deployment's origin, both name the issue, whose decision nodes the page reads. The release
-- before reads the first as no reference and the second as the issue's former decisions
-- document. Re-indexing either address can delete the issue's edge and add the document's.
--
-- Two queues, with the same columns and the same ordered index, the order the drain reads in:
--   - refs_decisions_forms_owed, the live queue, which the release before writes through the
--     triggers of 20261126040100-20261126040500 and no one else writes;
--   - refs_decisions_forms_seed_owed, the bootstrap queue, which only 20261126040600's seed
--     writes, for the text stored before this release.
-- They are two tables so that no key is written by both: the seed inserts thousands of rows in one
-- transaction, and a writer of the release before that relays a row meets any key the seed has
-- inserted and not yet committed, waits for the seed's commit, and can deadlock with it. The seed
-- writes a table no writer of either build inserts into, so it waits for no one and no one waits
-- for it. The drain reads both and a complete index of a source clears its row in each.
--
-- A queue of its own rather than a row of 20261117025900's refs_forms_owed, which that release
-- drains too: it settles a row of refs_forms_owed as soon as it has indexed the text itself, with
-- the grammar it knows, in the transaction that deleted the edges of this reading, so the text it
-- indexed leaves no row there for a build that reads the page to find. 20261126040100's trigger
-- relays each row a delete from refs_forms_owed removes into the live queue, the triggers of
-- 20261126040200-20261126040500 owe a source the one edge that release can add and delete none,
-- and this release deletes the rows a settle of its own relays, and the source's bootstrap row, in
-- a statement after the delete, in every transaction that indexed the source's whole text
-- (refs.queueSettleSourceOwed). So a row stays owed until a build that reads the page has indexed
-- that text itself, and only a settle by a build that does not clear these tables leaves a source
-- owed in them.
--
-- No `scope` kind, as refs_forms_owed has none: the reading changes no `#id`.
--
-- Its own migration: it creates tables and indexes and takes no existing table's lock, and each
-- migration after it holds one table's own (20261117025900's precedent).
create table refs_decisions_forms_owed (
  kind text not null check (kind in ('comment', 'ask', 'message', 'artifact')),
  id uuid not null,
  enqueued_at timestamptz not null default now(),
  primary key (kind, id)
);
create table refs_decisions_forms_seed_owed (
  kind text not null check (kind in ('comment', 'ask', 'message', 'artifact')),
  id uuid not null,
  enqueued_at timestamptz not null default now(),
  primary key (kind, id)
);
-- The indexes the drain reads through, on the whole order it reads in (refs.ListOwedAfter, `order
-- by enqueued_at, kind, id limit $1`): the seed gives every row of the corpus one enqueued_at, so
-- an index on that column alone would leave each pass sorting the entire backlog before its limit
-- could cut it (refs_forms_owed_enqueued's reason).
create index refs_decisions_forms_owed_enqueued on refs_decisions_forms_owed (enqueued_at, kind, id);
create index refs_decisions_forms_seed_owed_enqueued on refs_decisions_forms_seed_owed (enqueued_at, kind, id);
