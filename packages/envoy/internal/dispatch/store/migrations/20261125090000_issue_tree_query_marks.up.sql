-- 20261125090000_issue_tree_query_marks.up.sql — the queries of an issue a write left owed.
--
-- A write that moves the record of a query of a converted issue writes no version of that query
-- in its own transaction: in its batch it marks the issue's queries owed here (docs.QueueQueryMark),
-- with no statement of its own beside that batch's. The two result routes mark it (R12), and the
-- node PATCH and DELETE will (R7). After the write's COMMIT, and before the server's local publish
-- of the write's events, the drain (docs.Service.DrainOwedQueries) writes a version of each query
-- whose record moved and clears the mark, in one batch with its COMMIT; the tick of the
-- block-index reconciliation drains each mark a request left (docs.Service.ReconcileBlockIndex).
--
-- issue_query_marks holds one row for each issue:
--
--   issue_key       the issue whose queries are owed a version.
--   greatest_event  the greatest event id of the writes that marked it, null where none of them
--                   appended an event (a node PATCH or DELETE writes at graphstore.NoEvent). The
--                   drain causes what its versions state under it, and at the sentinel where it
--                   is null.
--   greatest_seq    the greatest graph_tuples.seq the writes that marked it appended. The drain
--                   reads it to order the marks it drains, it names the versions it writes after
--                   the writer of that tuple (graph_tuples.who), and it clears a mark only where the
--                   mark still holds the two causes it read, so a mark newer than its drain stays.
--
-- No key references issues: a key would take a lock of issues, which the pre-deploy census would
-- size as a table this migration touches. No writer deletes an issue.
--
-- Rolling deploy. The table is new and holds no row; the release before names it nowhere and
-- writes the version of a query a result moves in the result's own transaction. Rolling back past
-- this migration is an image rollback alone: a mark left behind is read by nothing that release
-- runs, and the tick of a build of this release drains it once it serves again.
create table issue_query_marks (
  issue_key text primary key,
  greatest_event bigint,
  greatest_seq bigint not null
);
