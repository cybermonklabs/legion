-- 20261110184749_requirement_target_status.up.sql
--
-- INFRA-2719: the Verified Linux spec loop reads its requirement lines two ways, by the target
-- the line is about and by the status it carries, and Dispatch serves neither. Today those two
-- reads fall back to GET /api/v1/search, which INFRA-988 measured at 84.2% of the whole
-- database's time at 1x and 371.4 ms a call, and which fails the 1,000 ms delivery bound at
-- every instance size that run tried (dispatch://INFRA-988/artifact/report-md). The route that
-- replaces it needs somewhere to filter, and the block index is the only table that already
-- holds one row per block of every current document and is kept in step by every accepted
-- write (refs/blocks.go).
--
-- These are the `requirement` block's own two attributes, added to the block schema in the same
-- change (pmdoc/schema/blocks.json, version 3 -> 4). They are named for the block type rather
-- than called `target` and `status`, because document_blocks holds a row for every block of
-- every type and a bare `status` on that table would read as the row's own state.
--
-- Both columns are nullable with no default, which in this Postgres is a catalogue write: the
-- table is not rewritten, no existing row is read, and every row written before this migration
-- carries null, which is exactly "this block declares no target" - true of every block that is
-- not a requirement, and of a requirement whose author wrote none. The alter takes ACCESS
-- EXCLUSIVE on document_blocks for that catalogue write, which conflicts with every reader and
-- every writer, so the window is the catalogue write alone, independent of the table's size.
--
-- The two indexes are partial on `type = 'requirement'` and on the column being present, so they
-- hold one entry per requirement that declares that attribute and nothing for the blocks of
-- every other type, which are almost all of them. They are plain `create index`: this unmarked
-- migration runs inside one transaction, where `concurrently` is refused. That takes a SHARE
-- lock on document_blocks for the build, which blocks writes to it - the block index's own
-- writes, not reads of any document - for as long as the build takes. The build scans the table
-- once; at the size production's block index reaches (the load test's corpus was 891 proposals,
-- 464,602 comment rows), that is seconds rather than minutes, and it is the reason this
-- migration adds the index with the column rather than leaving it to a later deploy: a second
-- ACCESS EXCLUSIVE window costs more than one.
--
-- Rollback: dropping the two columns drops the two indexes with them, and the route added in
-- this change answers nothing without them, so a build from before this migration is unaffected
-- - it reads neither column and serves neither route. A build from after it, on a schema
-- without them, fails both routes' statements and nothing else; no other read touches these
-- columns.
--
-- An alternative considered and not taken: one `attributes jsonb` column on document_blocks with
-- a GIN index, which would serve any future attribute of any block type without a migration.
-- The loop's two reads are equality on one key each, which a partial btree answers in a single
-- index scan and a GIN index answers less directly, and the whole point of this change is the
-- cost of the read. A third attribute wanting the same treatment is the moment to revisit it.

alter table document_blocks
  add column requirement_target text,
  add column requirement_status text;

create index document_blocks_requirement_target
  on document_blocks (requirement_target)
  where type = 'requirement' and requirement_target is not null;

create index document_blocks_requirement_status
  on document_blocks (requirement_status)
  where type = 'requirement' and requirement_status is not null;
