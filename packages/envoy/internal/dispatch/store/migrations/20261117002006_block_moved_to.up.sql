-- 20261117002006_block_moved_to.up.sql — where a block address that no longer stands moved to.
--
-- A move of a block from one document to another is a delete and an insert: document_blocks is
-- keyed (artifact_id, block_id) (0121_block_graph.up.sql) and a block's address is
-- `<ref_key>#<block id>` (text.TargetID), so the address changes with the document, and every
-- citation of the old address would dangle. No stored pointer is rewritten for it. Each move
-- appends one `moved_to` tuple instead, from the old address to the block it moved to
-- (graphstore.TupleMovedTo), and a reader that meets an address no document_blocks row holds
-- follows those tuples (dispatch://INFRA-1150 R8).
--
-- block_moved_to(address) is that one reading, for the three resolvers that reach a block:
-- refs.Closure and refs.BlockLinks call it in their own statements, and graphstore.MovedTo, the
-- reading of api's blockReferenceNode, calls it alone, so the rule is written once. It answers the
-- address a document_blocks row holds that the chain from address reaches: address itself when a
-- row holds it, else the far end of the newest moved_to that address states now - read through
-- graph_tuples_as_of_out as graphstore.AsOfOut reads a relation, the newest tuple of each far end
-- kept while it asserts - and so on from there. Each step leaves an address no row holds. It
-- follows at most 8 tuples (graphstore.MovedToHops), since dispatch://INFRA-1253 moves blocks
-- again and a chain is the normal case, and it answers null where the chain stops before a block
-- that stands: an address that states no moved_to, a far end that is no block, a cycle, or a
-- ninth tuple.
--
-- Rolling deploy. A new function and nothing else: the release before this one names no
-- block_moved_to, and nothing it runs changes. Rolling back past this migration is an image
-- rollback alone; the function left behind is called by nothing that release runs.
create function block_moved_to(address text) returns text
  language plpgsql stable strict
  as $$
declare
  reached text := address;
  passed text[] := array[address];
begin
  for hop in 0 .. 8 loop
    if exists (select 1 from document_blocks d where d.node_id = reached) then
      return reached;
    end if;
    exit when hop = 8;
    select moved.to_id into reached
    from (
      select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
      from graph_tuples t
      where t.from_kind = 'block' and t.from_id = reached and t.kind = 'moved_to'
      order by t.to_kind, t.to_id, t.at desc, t.seq desc
    ) moved
    where moved.asserted and moved.to_kind = 'block'
    order by moved.at desc, moved.seq desc
    limit 1;
    if not found or reached = any(passed) then
      return null;
    end if;
    passed := passed || reached;
  end loop;
  return null;
end
$$;
