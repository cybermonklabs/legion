-- 20261117002006_block_moved_to.up.sql — where a block address that no longer stands moved to.
--
-- A move of a block from one document to another is a delete and an insert: document_blocks is
-- keyed (artifact_id, block_id) (0121_block_graph.up.sql) and a block's address is
-- `<ref_key>#<block id>` (text.TargetID), so the address changes with the document, and every
-- citation of the old address would dangle. No stored pointer is rewritten for it. Each move
-- appends one `moved_to` tuple instead, from the old address to the block it moved to
-- (graphstore.TupleMovedTo), and a reader that meets an address no document_blocks row holds
-- follows those tuples.
--
-- moved_to_reach(address) is that one reading, for the three resolvers that reach a block:
-- refs.Closure and refs.BlockLinks call it in their own statements, and graphstore.MovedTo, the
-- reading of api's blockReferenceNode, calls it alone, so the rule is written once. It answers
-- what the chain from address reaches, as (kind, id): ('block', the address a document_blocks row
-- holds) - address itself when a row holds it, else the far end of the newest moved_to that
-- address states now, read through graph_tuples_as_of_out as graphstore.AsOfOut reads a relation,
-- the newest tuple of each far end kept while it asserts, and so on from there, each step leaving
-- an address no row holds. A moved_to whose far end is an ask node ends the chain at that ask,
-- ('ask', the ask's id), while its asks row stands: the heading of a decisions document's section
-- that cites an answered ask a person removed from its spec moves to the ask itself, whose
-- address still answers the person's answer. It follows at most 8 tuples (graphstore.MovedToHops),
-- since blocks are moved again later and a chain is the normal case, and it answers
-- nulls where the chain stops before a block that stands or an ask: an address that states no
-- moved_to, a far end that is neither, an ask no row holds, a cycle, or a ninth tuple.
--
-- block_moved_to(address) is the block moved_to_reach reaches, null where it reaches none: the
-- reading of a caller that wants a block and nothing else.
--
-- Rolling deploy. Two new functions and nothing else: the release before this one names neither,
-- and nothing it runs changes. Rolling back past this migration is an image rollback alone; the
-- functions left behind are called by nothing that release runs.
create function moved_to_reach(address text, out kind text, out id text)
  language plpgsql stable strict
  as $$
declare
  reached text := address;
  reached_kind text;
  passed text[] := array[address];
begin
  for hop in 0 .. 8 loop
    if exists (select 1 from document_blocks d where d.node_id = reached) then
      kind := 'block';
      id := reached;
      return;
    end if;
    exit when hop = 8;
    select moved.to_kind, moved.to_id into reached_kind, reached
    from (
      select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
      from graph_tuples t
      where t.from_kind = 'block' and t.from_id = reached and t.kind = 'moved_to'
      order by t.to_kind, t.to_id, t.at desc, t.seq desc
    ) moved
    where moved.asserted and moved.to_kind in ('block', 'ask')
    order by moved.at desc, moved.seq desc
    limit 1;
    if not found or reached = any(passed) then
      return;
    end if;
    if reached_kind = 'ask' then
      -- An ask node's id is its row's uuid; anything else names no ask, and is read as none
      -- rather than cast.
      if reached ~ '^[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}$' then
        if exists (select 1 from asks k where k.id = reached::uuid) then
          kind := 'ask';
          id := reached;
        end if;
      end if;
      return;
    end if;
    passed := passed || reached;
  end loop;
end
$$;

create function block_moved_to(address text) returns text
  language sql stable strict
  as $$
    select reach.id from moved_to_reach(address) reach where reach.kind = 'block'
  $$;
