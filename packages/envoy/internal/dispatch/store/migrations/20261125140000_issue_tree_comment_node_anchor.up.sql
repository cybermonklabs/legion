-- 20261125140000_issue_tree_comment_node_anchor.up.sql - the node a comment's anchor is on.
--
-- A comment can anchor on the text of a node that is no block of a document: a lens, a finding, a
-- risk, a constraint or a result of its issue (this issue's spec R31). Such an anchor is no mark of
-- a document, so comments.anchor, which holds a document's mark (model.Anchor), stays null on that
-- comment, and the anchor goes in a column of its own:
--
--   node_anchor   jsonb, null for every comment but one anchored on a node: node_kind and node_id,
--                 the node as graph_nodes keys it (a finding is kind block, id
--                 lens/<uuid>#<block id>); quote, the words it quotes; occurrence, the zero-based
--                 match of the quote in the node's text it names; and orphaned, true once a
--                 write of the node's text drops that match or a DELETE takes the node gone.
--
-- The comment's anchored_in and its part_of to that node are what the store holds of it; the
-- column is their projection, which graphstore.CommentAnchoredInSQL reads and the census
-- comment_anchor_block compares with the tuples. No head row reads it, so no trigger of
-- 20261123180100_issue_tree_head names it.
--
-- The column is nullable and has no default, so the ALTER is a catalog change: no stored row is
-- rewritten or read, and every row comments holds reads null in it.
--
-- Rolling deploy. The release before names the column nowhere: it reads a comment with a null
-- comments.anchor as one on its issue tied to no text, and a comment it writes holds a null
-- node_anchor. Rolling back past this migration is an image rollback alone; the rollback script
-- drops the column, and each comment on a node then reads as one on its issue.
set local lock_timeout = '500ms';

alter table comments add column node_anchor jsonb;
