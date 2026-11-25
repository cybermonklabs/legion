-- 20261125150000_issue_tree_block_node_listed.up.sql
--
-- block_node_listed, with the signature and the body of block_ref_listed
-- (20261101144855_block_pointer_links), for the tuple arm of graph_edges to test a pointer's target
-- through (dispatch://INFRA-1253/spec#b-r-block-type-supertag): 20261125150001 restates the view to
-- call it, and refs' blockRefListed and the store arm of its mention backlinks call it for a row that
-- names its node. block_ref_listed stays as it is for the arms over the rows that name no node,
-- since only such a row tests a target past the store's address bound. Until a boot step restates
-- block_node_listed to read the target's type from its node's supertag, which it does only once
-- every process of the fleet writes that supertag (the capability typed_block_supertags), it reads
-- document_blocks.type exactly as block_ref_listed does, so every pointer is listed as it was.
--
-- The migration creates one function and nothing else: it reads no stored row, and refuses and
-- rewrites none.
create function block_node_listed(to_types text[], to_id text) returns boolean language sql stable as $$
  select $1 is null or not exists (select 1 from document_blocks t where t.node_id = $2 and not (t.type = any($1)))
$$;
