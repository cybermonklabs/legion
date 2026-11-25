-- 20261120122600_block_index_marking_rule.up.sql — the block index's marking rule, recorded on
-- every mark, every event cursor and every backfill horizon of this build's.
--
-- The marking rule versions what a block-index pass and the block-index tick owe: which documents
-- a build marks owed a block index (document_block_versions.store_owed_at, migration
-- 20261116221539) and which comments its tick writes. The code declares it once
-- (refs.BlockIndexMarkingRule) and raises it whenever what a pass owes changes. Builds of two rules
-- must never take each other's place: a build that owes more than another must not start reading
-- the event log where the other's cursor stands, nor from a horizon the other's backfill recorded,
-- since that build and that backfill marked what their own rule owes; and a build of a lower rule
-- that drains a document a build of a higher rule marked, during a roll or after a rollback, must
-- leave that mark for the higher rule. Rule 1 is the first rule this code ships. This migration
-- adds three columns:
--
--   document_block_versions.store_owed_rule    the marking rule that set store_owed_at. Every
--                       statement that marks a document writes its build's rule there and keeps
--                       the higher of the stored rule and its own where a mark already stands; a
--                       pass of rule r clears the mark, and wins the revision fence at the version
--                       the row stands at, only where it is r or lower, or where the store last
--                       stated the document below that version (refs' queueStand), and lists a
--                       document a higher rule marked only there (docs' owedDocuments). The rule
--                       is read only where a mark stands: a pass that clears the mark leaves the
--                       rule as it was.
--   block_index_event_cursors.marking_rule     the rule of the build whose cursor the row is. A
--                       build with no row of its own starts at the newest cursor of a build of its
--                       own rule, else at the horizon of the newest backfill of its own rule, else
--                       at its first tick's safe horizon (docs' readEventsAfterCursor).
--   graph_backfill_horizon.marking_rule        the rule of the backfill whose horizon the row is,
--                       now the table's key: one row for each rule, and each run of
--                       `envoy-dispatch backfill-graph-store` replaces its own rule's row and
--                       leaves every other (graphcensus's recordBackfillHorizon). With one row, an
--                       older build's backfill run after a newer rule's would move the row, and the
--                       newer rule's first tick would pass over every event between the two.
--
-- Each is `integer not null default 1`. The release before this one and the trigger
-- document_block_versions_owe (migration 20261108070000) insert document_block_versions rows
-- without naming the column, and such a row holds rule 1, so any mark on it that no marking
-- statement of a later rule wrote is a rule-1 mark. A cursor row and a horizon row that stand
-- when this runs were written by a build of this code before this migration, which is rule 1's,
-- so they read rule 1 too, and the horizon's one row becomes rule 1's. The id column that held the
-- horizon to one row goes with its check and its key.
--
-- Two comments of the migrations this one follows state what this one makes false, and are left
-- as they are because a runner refuses an edit of the text of a migration a database has applied
-- unless the edit names the text it replaces (pgmigrate.Reconcile): 20261118115645's header says
-- a build with no cursor row starts at the earlier of the newest cursor another build left and the
-- backfill's horizon, and that graph_backfill_horizon holds one row that each run replaces; a
-- build now starts as the block_index_event_cursors entry above says, and the table holds one row
-- for each rule. 20261116221539's header says the block-index reconciliation lists every document
-- holding store_owed_at; it lists a document a higher rule marked only where the store last
-- stated it below its version.
--
-- Rolling deploy. The release before this one names none of the three columns and reads neither
-- block_index_event_cursors nor graph_backfill_horizon, so it runs on as it did. Postgres keeps a
-- constant default in the catalog, so each ADD COLUMN rewrites no row, and every row that stands
-- reads rule 1; the new key of graph_backfill_horizon builds an index over the one row it can
-- hold. The two small tables are altered first and document_block_versions last, so its ACCESS
-- EXCLUSIVE lock is held for its own catalog write alone: block_index_event_cursors and
-- graph_backfill_horizon are created by 20261118115645 in the same release and hold no row at
-- deploy, and document_block_versions held 9,881 rows on the load-test deployment and fewer in
-- production. Rolling back past this migration is an image rollback alone: a build of the release
-- before reads none of what it adds.
alter table graph_backfill_horizon
  add column marking_rule integer not null default 1,
  drop column id,
  add primary key (marking_rule);

alter table block_index_event_cursors add column marking_rule integer not null default 1;

alter table document_block_versions add column store_owed_rule integer not null default 1;
