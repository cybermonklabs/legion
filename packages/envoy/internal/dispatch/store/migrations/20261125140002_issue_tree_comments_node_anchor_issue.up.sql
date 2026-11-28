-- pgmigrate:non-transactional
-- 20261125140002_issue_tree_comments_node_anchor_issue.up.sql - the comments of an issue anchored
-- on a node, by that issue.
--
-- Every answer of the tree read carries each comment on the text of a node it returns, an agent's
-- comment that no person answered among them (this issue's spec R2, R31). The read finds such a
-- comment on a document through the anchor's document (comments_anchor_artifact), and one on a
-- lens, a finding, a risk, a constraint or a result through its issue: its arm reads the comments
-- of the issue whose comments.node_anchor is set (api's textCommentsStatement). This index holds
-- only those comments, keyed by issue as that arm narrows to them, so the arm is one probe of it
-- rather than a read of every comment of the issue. Its predicate is the arm's own, which is what
-- lets the planner prove the arm keeps to it.
--
-- comments grows with every comment. A concurrent build does not hold off its writers while it
-- scans the table, so the migration runs outside a transaction, alone in its file (pgmigrate's
-- ConcurrentIndex, R19). It is a performance index: no constraint and no later migration depends
-- on it, and the release before writes no node anchor. A failed concurrent build leaves an INVALID
-- index of this name on comments, which the runner drops with DROP INDEX CONCURRENTLY and builds
-- again on the next attempt.
create index concurrently if not exists comments_issue_node_anchor
    on comments (issue_key)
    where node_anchor is not null;
