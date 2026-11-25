-- 20261118115645_block_index_event_cursors.up.sql
-- How far each build's block-index reconciliation has read the event log, and where the backfill of
-- the node and tuple store began reading it. The release before this one writes a version's rows,
-- and a new document's, the spec of an issue it creates included, and a comment's row with no node,
-- states nothing in the node and tuple store, and neither reads nor sets this build's own mark
-- (store_owed_at, migration 20261116221539). So a document it edits or creates after this build's
-- drain has cleared that mark, and a comment it writes after the backfill, is owed this build a
-- pass that nothing lists. Every block-index tick of a build therefore reads the events after its
-- build's cursor, whether or not another build serves, before it lists the owed documents (docs'
-- readEventsAfterCursor): it marks owed every document an artifact.version or artifact.created
-- event names, and every document of each issue an issue.created event names, whose
-- document_block_versions.store_version is not its version, and gives every comment a
-- comment.created or comment.answered event names whose row names no node the backfill's own write.
--
-- block_index_event_cursors holds one row per build commit:
--
--   build_commit  the commit the build was stamped with (cmd/dispatch's buildCommit), the empty
--                 string for an unstamped build. The build's first tick that reads a proven safe
--                 horizon inserts the row at the earlier of the highest event_id another build's
--                 row holds and the event_id of graph_backfill_horizon, where both exist; at
--                 whichever one exists where only one does; and at that horizon where neither
--                 does, so the cursor never starts at the beginning of the log. A build that holds
--                 a row starts at its own row, however old.
--   event_id      the highest events.id the build's ticks have read or passed. A tick reads the
--                 events after it and at or below the event log's safe horizon (events.Horizon),
--                 at most one page of them, and never moves it past that horizon: event ids are not
--                 commit order, so an uncapped `id > event_id` read could pass an event that is
--                 still committing.
--
-- graph_backfill_horizon holds one row, the horizon the newest run of `envoy-dispatch
-- backfill-graph-store` began at (graphcensus's recordBackfillHorizon):
--
--   event_id      an id of the event log at or below which every event was final before the run
--                 read its first row (events.Horizon.WriterFloor), so every event the run cannot
--                 have seen is above it. A build with no cursor row of its own starts no later than
--                 it (above), so nothing the release before writes between the backfill and that
--                 build's first tick is passed over. Each run replaces it: the newest run read every
--                 row the runs before it read.
--   recorded_at   when the run recorded it.
--
-- Two new tables, of one short row per build deployed and one row; the previous release never
-- reads either.
create table block_index_event_cursors (
  build_commit text primary key,
  event_id bigint not null
);

create table graph_backfill_horizon (
  id smallint primary key check (id = 1),
  event_id bigint not null,
  recorded_at timestamptz not null default now()
);
