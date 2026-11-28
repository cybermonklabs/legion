-- pgmigrate:non-transactional
-- 20261125140001_issue_tree_comments_node_anchor_node.up.sql - the comments anchored on a node, by
-- that node.
--
-- A write that takes a lens, a finding, a risk or a constraint gone (the DELETE of this issue's
-- spec R7), and a write that appends a new text value to such a node, orphans in its own
-- transaction each comment whose comments.node_anchor names that node (R31: docs'
-- ReadGoneNodeAnchors and RefreshNodeAnchors). Each reads those comments by the node's kind and id,
-- while it holds the owner lock every comment and node write of the issue waits on. This index
-- holds only the comments with a node anchor, keyed as those reads narrow to them, so each read is
-- one probe of it rather than a read of every comment. Its expressions and its predicate are the
-- reads' own (docs' nodeAnchorsOnStatement), which is what lets the planner prove they keep to it.
--
-- comments grows with every comment. A concurrent build does not hold off its writers while it
-- scans the table, so the migration runs outside a transaction, alone in its file, as the census
-- admits a concurrent build of any table's size (pgmigrate's ConcurrentIndex, R19). It is a
-- performance index: no constraint and no later migration depends on it, and the release before
-- writes no node anchor. A failed concurrent build leaves an INVALID index of this name on
-- comments, which the runner drops with DROP INDEX CONCURRENTLY and builds again on the next
-- attempt.
create index concurrently if not exists comments_node_anchor_node
    on comments ((node_anchor->>'node_kind'), (node_anchor->>'node_id'))
    where node_anchor is not null;
