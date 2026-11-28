-- 20261128054905_issue_tree_head_view_indexes.up.sql — the indexes a view's read of its head rows
-- finds the blocks it answers through.
--
-- A view of an issue's tree (GET /api/v1/issues/{key}/tree?view=...) reads, of the blocks of the
-- issue's own documents, the typed blocks of its types and the first block under the issue's node
-- (refs' headRowsView). issue_head_nodes (20261127120803_issue_tree_head) holds only its primary
-- key and issue_head_nodes_node, so the read took every block of the issue's own documents through
-- the key and tested each one: the spec view of an issue whose 65 other documents hold 45,008
-- paragraphs spent 40 ms of its 41 ms of database time there, and the spec view of an issue of the
-- largest plan tested the plan's 2,786 blocks. Each index below holds one kind of block the read
-- answers, and of the others only the few the read must test, so the read reads the blocks it
-- answers:
--
--   issue_head_nodes_supertag   the blocks of each issue by their supertag, byte by byte (collate
--                               "C"), so the blocks whose supertag begins with a view's type are
--                               one range of it: those of the type, and those of a longer name,
--                               whose text the read tests for the type's opening.
--   issue_head_nodes_block_type the blocks of each issue by their document_blocks type, of the
--                               blocks whose type is not their supertag: a block whose type is its
--                               supertag is found by its supertag.
--   issue_head_nodes_opened     the blocks of each issue whose text holds `:::` other than as the
--                               opening of a typed block of their own supertag at its start,
--                               `:::<supertag>{`, and as fences before white space or the end,
--                               which close it: the blocks whose text may open a typed block of a
--                               view's type that neither their supertag nor their type names. A
--                               typed block whose text holds no other fence opens no type but its
--                               own, and the read finds it by its supertag.
--   issue_head_nodes_fragment   the blocks of each issue by their document, which finds the blocks
--                               of a child issue's own fragment of its tree (issue_trees).
--   issue_head_nodes_children   the blocks under each issue's node in the order of an issue's
--                               children (refs' treeChildOrder): by their rank, byte by byte, then
--                               by their address, so the first ranked one is one entry of it.
--   issue_head_nodes_foreign    the blocks of each issue whose address does not begin with the
--                               issue's key and `/`, so are no blocks of the issue's own documents:
--                               a finding of a lens and a block of another issue's document placed
--                               under a node of the issue, which a view and the read with no view
--                               read whole. As two ranges of the key, below `<key>/` and from
--                               `<key>0` on, whose bounds the read's plan does not know, Postgres
--                               read each in a scan of the whole table where one issue held most
--                               of its rows.
--
-- Each is partial to the blocks it finds, so a writer of other rows writes no entry of it, and no
-- read of other rows can take it for the primary key.
--
-- Rolling deploy. issue_head_nodes is 20261127120803's, which ships in this same release, so it
-- holds no row when this migration applies, and each index is built over an empty table. The
-- release before names neither the table nor an index of it. Rolling back past this migration is
-- an image rollback alone, and the rollback script drops the indexes.
create index issue_head_nodes_supertag on issue_head_nodes (issue_key, supertag collate "C")
  where node_kind = 'block' and supertag is not null;

create index issue_head_nodes_block_type on issue_head_nodes (issue_key, block_type)
  where node_kind = 'block' and block_type is not null and block_type is distinct from supertag;

create index issue_head_nodes_opened on issue_head_nodes (issue_key)
  where node_kind = 'block' and position(':::' in coalesce(text, '')) > 0
    and (supertag is null or not starts_with(text, ':::' || supertag || '{') or substr(text, 2) ~ ':::\S');

create index issue_head_nodes_fragment on issue_head_nodes (issue_key, document_id)
  where node_kind = 'block' and document_id is not null;

create index issue_head_nodes_children on issue_head_nodes (issue_key, parent_address, rank collate "C", node_address)
  where node_kind = 'block' and parent_kind = 'issue';

create index issue_head_nodes_foreign on issue_head_nodes (issue_key)
  where node_kind = 'block' and not starts_with(node_address, (issue_key || '/') collate "C");
